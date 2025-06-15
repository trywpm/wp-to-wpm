package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

const (
	pluginRepoURL     = "https://plugins.svn.wordpress.org"
	themeRepoURL      = "https://themes.svn.wordpress.org"
	defaultWorkers    = 10
	requestTimeout    = 30 * time.Second
	pluginsJSONFile   = "plugins.json"
	themesJSONFile    = "themes.json"
	conflictsJSONFile = "conflicts.json"
)

var (
	log        = logrus.New()
	nameReg    = regexp.MustCompile(`^[\w-]{3,164}$`)
	httpClient = &http.Client{Timeout: requestTimeout}
)

// APIResponse defines the structure for the version field from the WP API.
type APIResponse struct {
	Version string `json:"version"`
}

// QualifiedPackage holds the result from a worker.
type QualifiedPackage struct {
	Name    string
	Version string
	Type    string
	Error   error
}

// PackageResult represents the outcome of checking a package
type PackageResult struct {
	Name       string
	Version    string
	Type       string
	Status     string // "qualified", "not_found", "error", "existing"
	StatusCode int
	Error      error
}

func setupLogger(verbose bool) {
	log.SetOutput(os.Stdout)
	if verbose {
		log.SetLevel(logrus.DebugLevel)
	} else {
		log.SetLevel(logrus.InfoLevel)
	}
	log.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
		ForceColors:   true,
	})
}

// listSVNPackages fetches a list of directories from an SVN repository.
func listSVNPackages(ctx context.Context, svnRepoURL string) ([]string, error) {
	l := log.WithField("repo", svnRepoURL)
	l.Info("📋 Listing packages from SVN...")

	cmd := exec.CommandContext(ctx, "svn", "list", svnRepoURL)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to get stdout pipe for svn list: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start svn list command: %w", err)
	}

	var packageNames []string
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if name := strings.TrimRight(line, "/"); name != "" {
			packageNames = append(packageNames, name)
		}
	}

	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("svn list command failed for %s: %w", svnRepoURL, err)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading svn list output: %w", err)
	}

	l.Infof("✅ Found %d total entries from SVN.", len(packageNames))
	return packageNames, nil
}

// fetchLatestVersion qualifies a package by checking the WP API.
func fetchLatestVersion(ctx context.Context, packageName, packageType string) (*PackageResult, error) {
	var apiURL string
	if packageType == "theme" {
		apiURL = fmt.Sprintf("https://api.wordpress.org/themes/info/1.2/?action=theme_information&slug=%s", packageName)
	} else {
		apiURL = fmt.Sprintf("https://api.wordpress.org/plugins/info/1.2/?action=plugin_information&slug=%s", packageName)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return &PackageResult{
			Name:   packageName,
			Type:   packageType,
			Status: "error",
			Error:  fmt.Errorf("failed to create api request: %w", err),
		}, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		log.WithFields(logrus.Fields{
			"package": packageName,
			"type":    packageType,
		}).Warnf("⚠️ API request failed: %v", err)
		return &PackageResult{
			Name:   packageName,
			Type:   packageType,
			Status: "error",
			Error:  fmt.Errorf("api request failed: %w", err),
		}, err
	}
	defer resp.Body.Close()

	result := &PackageResult{
		Name:       packageName,
		Type:       packageType,
		StatusCode: resp.StatusCode,
	}

	// 404 means the package doesn't exist - should be removed from existing lists
	if resp.StatusCode == 404 {
		log.WithFields(logrus.Fields{
			"package": packageName,
			"type":    packageType,
		}).Debug("🔍 Package not found (404) - will be removed from existing list")
		result.Status = "not_found"
		return result, fmt.Errorf("package not found (status 404)")
	}

	// Other 4xx errors are client errors - log them but don't remove from existing lists
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		log.WithFields(logrus.Fields{
			"package": packageName,
			"type":    packageType,
			"status":  resp.StatusCode,
		}).Warnf("⚠️ Client error from WordPress API (status %d)", resp.StatusCode)
		result.Status = "error"
		return result, fmt.Errorf("api client error (status %d)", resp.StatusCode)
	}

	// 5xx errors are server issues and might be temporary. We log these.
	if resp.StatusCode >= 500 {
		log.WithFields(logrus.Fields{
			"package": packageName,
			"type":    packageType,
			"status":  resp.StatusCode,
		}).Errorf("❌ WordPress API returned a server error (status %d)", resp.StatusCode)
		result.Status = "error"
		return result, fmt.Errorf("api server error (status %d)", resp.StatusCode)
	}

	bodyBytes, _ := io.ReadAll(resp.Body)
	var apiResp APIResponse
	if err := json.Unmarshal(bodyBytes, &apiResp); err != nil {
		log.WithFields(logrus.Fields{
			"package": packageName,
			"type":    packageType,
		}).Warnf("⚠️ Failed to decode API response: %v", err)
		result.Status = "error"
		result.Error = fmt.Errorf("failed to decode api response: %w", err)
		return result, err
	}

	if apiResp.Version == "" {
		log.WithFields(logrus.Fields{
			"package": packageName,
			"type":    packageType,
		}).Warn("⚠️ API response did not contain a version")
		result.Status = "error"
		result.Error = fmt.Errorf("api response did not contain a version")
		return result, result.Error
	}

	result.Status = "qualified"
	result.Version = apiResp.Version
	return result, nil
}

// qualificationWorker processes package names from a channel to see if they are valid.
func qualificationWorker(ctx context.Context, jobs <-chan [2]string, results chan<- *QualifiedPackage, wg *sync.WaitGroup) {
	defer wg.Done()
	for job := range jobs {
		packageName, packageType := job[0], job[1]

		select {
		case <-ctx.Done():
			return
		default:
		}

		// Match the package name against the regex.
		if !nameReg.MatchString(packageName) {
			continue
		}

		// Check with the WordPress API.
		result, err := fetchLatestVersion(ctx, packageName, packageType)
		if err != nil {
			// Only send the result if it's a 404 (not_found) so we can remove it from existing lists
			if result != nil && result.Status == "not_found" {
				results <- &QualifiedPackage{
					Name:  packageName,
					Type:  packageType,
					Error: err,
				}
			}
			continue
		}

		results <- &QualifiedPackage{
			Name:    packageName,
			Version: result.Version,
			Type:    packageType,
		}
	}
}

// sortJson sorts a map by keys and returns the map with sorted keys.
func sortJson(data map[string]string) map[string]string {
	sortedData := make(map[string]string)
	sortedKeys := slices.Sorted(maps.Keys(data))

	for _, k := range sortedKeys {
		sortedData[k] = data[k]
	}

	return sortedData
}

// saveJSON saves a map to a file in indented JSON format.
func saveJSON(filePath string, data interface{}) error {
	file, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal data for %s: %w", filePath, err)
	}
	return os.WriteFile(filePath, file, 0644)
}

// loadExistingJSON loads an existing JSON file into a map
func loadExistingJSON(filePath string) (map[string]string, error) {
	data := make(map[string]string)

	// Check if file exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		log.WithField("file", filePath).Info("📄 No existing file found, starting fresh")
		return data, nil
	}

	file, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read existing file %s: %w", filePath, err)
	}

	if err := json.Unmarshal(file, &data); err != nil {
		return nil, fmt.Errorf("failed to unmarshal existing file %s: %w", filePath, err)
	}

	log.WithFields(logrus.Fields{
		"file":  filePath,
		"count": len(data),
	}).Info("📄 Loaded existing data")

	return data, nil
}

func runGenerator(cmd *cobra.Command, args []string) error {
	workers, _ := cmd.Flags().GetInt("workers")
	verbose, _ := cmd.Flags().GetBool("verbose")
	setupLogger(verbose)

	log.Info("📄 Loading existing data...")
	existingPlugins, err := loadExistingJSON(pluginsJSONFile)
	if err != nil {
		return fmt.Errorf("failed to load existing plugins: %w", err)
	}

	existingThemes, err := loadExistingJSON(themesJSONFile)
	if err != nil {
		return fmt.Errorf("failed to load existing themes: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Warn("🛑 Signal received, shutting down workers.")
		cancel()
	}()

	var allPlugins, allThemes []string
	var wgList sync.WaitGroup
	var errPlugins, errThemes error

	wgList.Add(2)
	go func() {
		defer wgList.Done()
		allPlugins, errPlugins = listSVNPackages(ctx, pluginRepoURL)
	}()
	go func() {
		defer wgList.Done()
		allThemes, errThemes = listSVNPackages(ctx, themeRepoURL)
	}()
	wgList.Wait()

	if errPlugins != nil {
		return errPlugins
	}
	if errThemes != nil {
		return errThemes
	}

	log.Infof("👷 Qualifying %d plugins and %d themes with %d workers...", len(allPlugins), len(allThemes), workers)

	jobs := make(chan [2]string, len(allPlugins)+len(allThemes))
	results := make(chan *QualifiedPackage, len(allPlugins)+len(allThemes))
	var wgQualify sync.WaitGroup

	for range workers {
		wgQualify.Add(1)
		go qualificationWorker(ctx, jobs, results, &wgQualify)
	}

	for _, p := range allPlugins {
		jobs <- [2]string{p, "plugin"}
	}
	for _, t := range allThemes {
		jobs <- [2]string{t, "theme"}
	}
	close(jobs)

	wgQualify.Wait()
	close(results)

	plugins := make(map[string]string)
	themes := make(map[string]string)

	// Copy existing data
	maps.Copy(plugins, existingPlugins)
	maps.Copy(themes, existingThemes)

	var removedCount, addedCount, updatedCount int

	// Process results
	for res := range results {
		if res.Error != nil {
			// This means it's a 404 - remove from existing lists
			if res.Type == "plugin" {
				if _, exists := plugins[res.Name]; exists {
					delete(plugins, res.Name)
					removedCount++
					log.WithFields(logrus.Fields{
						"package": res.Name,
						"type":    res.Type,
					}).Debug("🗑️ Removed package (404 from API)")
				}
			} else {
				if _, exists := themes[res.Name]; exists {
					delete(themes, res.Name)
					removedCount++
					log.WithFields(logrus.Fields{
						"package": res.Name,
						"type":    res.Type,
					}).Debug("🗑️ Removed package (404 from API)")
				}
			}
		} else {
			if res.Type == "plugin" {
				if existingVersion, exists := plugins[res.Name]; exists {
					if existingVersion != res.Version {
						updatedCount++
						log.WithFields(logrus.Fields{
							"package":    res.Name,
							"oldVersion": existingVersion,
							"newVersion": res.Version,
						}).Debug("🔄 Updated package version")
					}
				} else {
					addedCount++
					log.WithFields(logrus.Fields{
						"package": res.Name,
						"version": res.Version,
					}).Debug("✅ Added new package")
				}
				plugins[res.Name] = res.Version
			} else {
				if existingVersion, exists := themes[res.Name]; exists {
					if existingVersion != res.Version {
						updatedCount++
						log.WithFields(logrus.Fields{
							"package":    res.Name,
							"oldVersion": existingVersion,
							"newVersion": res.Version,
						}).Debug("🔄 Updated package version")
					}
				} else {
					addedCount++
					log.WithFields(logrus.Fields{
						"package": res.Name,
						"version": res.Version,
					}).Debug("✅ Added new package")
				}
				themes[res.Name] = res.Version
			}
		}
	}

	log.WithFields(logrus.Fields{
		"added":   addedCount,
		"updated": updatedCount,
		"removed": removedCount,
	}).Info("📊 Processing summary")

	log.Infof("✅ Qualification complete. Found %d qualified plugins and %d qualified themes.", len(plugins), len(themes))

	// Find conflicts
	var conflicts []string
	if len(plugins) > 0 {
		for name := range themes {
			if _, exists := plugins[name]; exists {
				conflicts = append(conflicts, name)
			}
		}
	}

	// Remove conflicts from both maps
	if len(conflicts) > 0 {
		log.Warnf("⚠️ Found %d naming conflicts. Removing them from both lists.", len(conflicts))
		for _, name := range conflicts {
			delete(plugins, name)
			delete(themes, name)
		}
	}

	log.Info("💾 Saving output files...")
	if err := saveJSON(pluginsJSONFile, sortJson(plugins)); err != nil {
		return err
	}
	log.Infof("   -> Saved %d plugins to %s", len(plugins), pluginsJSONFile)

	if err := saveJSON(themesJSONFile, sortJson(themes)); err != nil {
		return err
	}
	log.Infof("   -> Saved %d themes to %s", len(themes), themesJSONFile)

	if len(conflicts) > 0 {
		slices.Sort(conflicts)
	}

	if err := saveJSON(conflictsJSONFile, conflicts); err != nil {
		return err
	}
	log.Infof("   -> Saved %d conflicts to %s", len(conflicts), conflictsJSONFile)

	log.Info("🎉 Generation complete!")
	return nil
}

func main() {
	rootCmd := &cobra.Command{
		Use:           "plugins-themes-json-generator",
		Short:         "Generates JSON files of qualified WordPress plugins and themes.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runGenerator,
	}

	rootCmd.Flags().IntP("workers", "w", defaultWorkers, "Number of parallel workers for API checks.")
	rootCmd.Flags().BoolP("verbose", "v", false, "Enable verbose logging.")

	if err := rootCmd.Execute(); err != nil {
		log.Errorf("❌ Error: %v", err)
		os.Exit(1)
	}
}
