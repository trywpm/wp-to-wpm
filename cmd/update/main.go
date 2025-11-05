package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

const (
	themeRepoURL   = "https://themes.svn.wordpress.org"
	pluginRepoURL  = "https://plugins.svn.wordpress.org"
	themeAPIURL    = "https://api.wordpress.org/themes/info/1.2/?action=theme_information&slug="
	pluginAPIURL   = "https://api.wordpress.org/plugins/info/1.2/?action=plugin_information&slug="
	resolvedJSON   = "resolved.json"
	conflictsJSON  = "conflicts.json"
	pluginsJSON    = "plugins.json"
	themesJSON     = "themes.json"
	maxRetries     = 3
	baseBackoff    = 5 * time.Second
	defaultWorkers = 50
	progressChunk  = 1000
)

var (
	log          = logrus.New()
	httpClient   = &http.Client{Timeout: 30 * time.Second}
	pkgNameRegex = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

type resolvedConfig struct {
	Plugins []string `json:"plugins"`
	Themes  []string `json:"themes"`
}

func setupLogger() {
	log.SetOutput(os.Stdout)
	log.SetLevel(logrus.InfoLevel)
	log.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   false,
		TimestampFormat: "2006-01-02 15:04:05",
		ForceColors:     true,
	})
}

func getSvnList(ctx context.Context, repoURL string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "svn", "list", repoURL)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("svn list for repo %s failed: %w\noutput: %s", repoURL, err, string(output))
	}

	var list []string
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := strings.Trim(scanner.Text(), "/ \r\n")
		if line != "" {
			list = append(list, line)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read svn list output: %w", err)
	}

	sort.Strings(list)
	return list, nil
}

func filterValidSlugs(slugs []string, re *regexp.Regexp) []string {
	var filtered []string
	for _, s := range slugs {
		if re.MatchString(s) && len(s) >= 3 && len(s) <= 164 {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

func loadJSONFile(path string, v interface{}) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// For optional files like resolved.json, we don't treat this as a fatal error.
		// We log it and the calling function will proceed with empty data.
		log.Warnf("file %s not found, proceeding with empty data.", path)
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read file %s: %w", path, err)
	}

	// Handle empty file case
	if len(data) == 0 {
		log.Warnf("file %s is empty, proceeding with empty data.", path)
		return nil
	}

	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("failed to parse json from %s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, data interface{}) error {
	dir := os.TempDir()
	tempFile, err := os.CreateTemp(dir, "temp-*.json")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}

	encoder := json.NewEncoder(tempFile)
	encoder.SetIndent("", "  ")

	if err := encoder.Encode(data); err != nil {
		tempFile.Close()
		os.Remove(tempFile.Name())
		return fmt.Errorf("failed to write json to temp file %s: %w", tempFile.Name(), err)
	}

	if err := tempFile.Close(); err != nil {
		os.Remove(tempFile.Name())
		return fmt.Errorf("failed to close temp file %s: %w", tempFile.Name(), err)
	}

	if err := os.Rename(tempFile.Name(), path); err == nil {
		return nil
	}

	source, err := os.Open(tempFile.Name())
	if err != nil {
		return fmt.Errorf("failed to open source temp file for copying: %w", err)
	}
	defer source.Close()

	destination, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create destination file for copying: %w", err)
	}
	defer destination.Close()

	_, err = io.Copy(destination, source)
	if err != nil {
		return fmt.Errorf("failed to copy temp file to destination: %w", err)
	}

	os.Remove(tempFile.Name())

	return nil
}

func checkSlugExists(ctx context.Context, apiURL, slug string) bool {
	url := apiURL + slug
	for attempt := 0; attempt < maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
		if err != nil {
			log.WithError(err).WithField("slug", slug).Warn("failed to create request")
			return false // Don't retry on request creation failure
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			log.WithError(err).WithField("slug", slug).Warnf("request failed, retrying...")
			time.Sleep(baseBackoff) // Generic backoff for network errors
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			return true
		}

		if resp.StatusCode >= 500 && resp.StatusCode < 600 {
			log.Warnf("server error (%d) for %s, retrying (%d/%d)...", resp.StatusCode, url, attempt+1, maxRetries)
			time.Sleep(time.Duration(attempt+1) * baseBackoff)
		} else {
			// Any other status code (e.g., 404) is a definitive failure
			return false
		}
	}
	log.Errorf("exceeded maximum retries for %s, marking as invalid.", url)
	return false
}

func slugValidatorWorker(
	ctx context.Context,
	jobs <-chan string,
	results chan<- string,
	wg *sync.WaitGroup,
	processed *atomic.Int64,
	total int,
	apiURL string,
) {
	defer wg.Done()
	for slug := range jobs {
		if checkSlugExists(ctx, apiURL, slug) {
			results <- slug
		}
		count := processed.Add(1)
		if count%progressChunk == 0 || int(count) == total {
			log.Infof("progress: processed %d / %d items.", count, total)
		}
	}
}

// sliceToSet converts a string slice to a map for efficient lookups.
func sliceToSet(slice []string) map[string]struct{} {
	set := make(map[string]struct{}, len(slice))
	for _, item := range slice {
		set[item] = struct{}{}
	}
	return set
}

func runUpdater(cmd *cobra.Command, args []string) error {
	setupLogger()

	pkgType, _ := cmd.Flags().GetString("type")
	workers, _ := cmd.Flags().GetInt("workers")

	if _, err := exec.LookPath("svn"); err != nil {
		return fmt.Errorf("svn command not found in PATH")
	}

	var resolvedConf resolvedConfig
	if err := loadJSONFile(resolvedJSON, &resolvedConf); err != nil {
		return err
	}

	var conflicts []string
	if err := loadJSONFile(conflictsJSON, &conflicts); err != nil {
		return fmt.Errorf("failed to read conflicts file: %w", err)
	}
	log.Infof("successfully loaded %d conflicts from %s", len(conflicts), conflictsJSON)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	var themes, plugins []string
	var themesErr, pluginsErr error

	log.Info("fetching themes and plugins lists from svn...")
	wg.Add(2)
	go func() {
		defer wg.Done()
		themes, themesErr = getSvnList(ctx, themeRepoURL)
	}()
	go func() {
		defer wg.Done()
		plugins, pluginsErr = getSvnList(ctx, pluginRepoURL)
	}()
	wg.Wait()

	if themesErr != nil {
		return fmt.Errorf("could not fetch themes list: %w", themesErr)
	}
	if pluginsErr != nil {
		return fmt.Errorf("could not fetch plugins list: %w", pluginsErr)
	}
	log.Info("successfully fetched svn lists.")

	// filter through regex to ensure valid package names
	validFormatThemes := filterValidSlugs(themes, pkgNameRegex)
	validFormatPlugins := filterValidSlugs(plugins, pkgNameRegex)
	if len(validFormatThemes) == 0 || len(validFormatPlugins) == 0 {
		return fmt.Errorf("list is empty after regex filtering, cannot proceed")
	}

	var slugsToValidate, resolvedSlugs []string
	var apiURL, outputFilename string

	if pkgType == "plugin" {
		slugsToValidate = validFormatPlugins
		resolvedSlugs = resolvedConf.Plugins
		apiURL = pluginAPIURL
		outputFilename = pluginsJSON
	} else {
		slugsToValidate = validFormatThemes
		resolvedSlugs = resolvedConf.Themes
		apiURL = themeAPIURL
		outputFilename = themesJSON
	}

	totalToProcess := len(slugsToValidate)
	log.Infof("starting remote validation for %d %s(s) with %d workers...", totalToProcess, pkgType, workers)

	jobs := make(chan string, totalToProcess)
	results := make(chan string, totalToProcess)
	var processedCount atomic.Int64

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go slugValidatorWorker(ctx, jobs, results, &wg, &processedCount, totalToProcess, apiURL)
	}

	for _, slug := range slugsToValidate {
		jobs <- slug
	}
	close(jobs)
	wg.Wait()
	close(results)

	var validatedSlugs []string
	for slug := range results {
		validatedSlugs = append(validatedSlugs, slug)
	}
	log.Infof("remote validation complete. found %d valid slugs.", len(validatedSlugs))
	log.Info("filtering validated slugs based on conflicts and resolutions...")

	validatedSlugsSet := sliceToSet(validatedSlugs)

	// find intersection of resolvedSlugs and validatedSlugs
	var concreteResolvedSlugs []string
	for _, slug := range resolvedSlugs {
		if _, found := validatedSlugsSet[slug]; found {
			concreteResolvedSlugs = append(concreteResolvedSlugs, slug)
		}
	}

	// remove from conflicts any slug that is in concreteResolvedSlugs
	concreteResolvedSet := sliceToSet(concreteResolvedSlugs)
	var conflictsToRemove []string
	for _, slug := range conflicts {
		if _, found := concreteResolvedSet[slug]; !found {
			conflictsToRemove = append(conflictsToRemove, slug)
		}
	}

	// remove conflictsToRemove from validatedSlugs
	conflictsToRemoveSet := sliceToSet(conflictsToRemove)
	var finalSlugs []string
	for _, slug := range validatedSlugs {
		if _, found := conflictsToRemoveSet[slug]; !found {
			finalSlugs = append(finalSlugs, slug)
		}
	}
	sort.Strings(finalSlugs)

	if err := writeJSON(outputFilename, finalSlugs); err != nil {
		return fmt.Errorf("failed to write final list: %w", err)
	}

	log.Infof("processing complete. updated %s with %d final slugs.", outputFilename, len(finalSlugs))
	return nil
}

func main() {
	rootCmd := &cobra.Command{
		Use:           "wp-list-updater",
		Short:         "Updates lists of available WordPress plugins and themes.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runUpdater,
	}

	rootCmd.Flags().StringP("type", "t", "", "type to process: 'plugin' or 'theme' (required)")
	rootCmd.Flags().IntP("workers", "w", defaultWorkers, "number of parallel validation workers")
	_ = rootCmd.MarkFlagRequired("type")

	if err := rootCmd.Execute(); err != nil {
		log.Errorf("❌ %v", err)
		os.Exit(1)
	}
}
