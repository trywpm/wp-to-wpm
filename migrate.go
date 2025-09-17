package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Masterminds/semver/v3"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

const (
	defaultWorkers    = 5
	defaultTagTimeout = 5 * time.Minute
	pluginRepo        = "https://plugins.svn.wordpress.org"
	themeRepo         = "https://themes.svn.wordpress.org"
	pluginApi         = "https://api.wordpress.org/plugins/info/1.2/?action=plugin_information&slug="
	themeApi          = "https://api.wordpress.org/themes/info/1.2/?action=theme_information&slug="
	pluginsJSONFile   = "plugins.json"
	themesJSONFile    = "themes.json"
	pluginRevFile     = ".plugin_last_rev"
	themeRevFile      = ".theme_last_rev"
)

var (
	log        = logrus.New()
	httpClient = &http.Client{Timeout: 30 * time.Second}
)

type SvnLog struct {
	Entries []SvnLogEntry `xml:"logentry"`
}

type SvnLogEntry struct {
	Revision string    `xml:"revision,attr"`
	Paths    []SvnPath `xml:"paths>path"`
}

type SvnPath struct {
	Path string `xml:",chardata"`
}

func loadPackageList(path string) ([]string, error) {
	var packages []string
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read package list %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &packages); err != nil {
		return nil, fmt.Errorf("failed to parse package list %s: %w", path, err)
	}
	return packages, nil
}

func readLastRevision(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("state file '%s' not found or unreadable: %w", path, err)
	}
	revStr := strings.TrimSpace(string(data))
	rev, err := strconv.Atoi(revStr)
	if err != nil {
		return 0, fmt.Errorf("invalid revision number in %s: %w", path, err)
	}
	return rev, nil
}

func writeLastRevision(path string, revision int) error {
	data := []byte(strconv.Itoa(revision))
	return os.WriteFile(path, data, 0644)
}

func setupLogger(verbose bool) {
	log.SetOutput(os.Stdout)
	log.SetLevel(logrus.InfoLevel)
	log.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   verbose,
		TimestampFormat: "2006-01-02 15:04:05",
		ForceColors:     true,
	})
	log.Info("📝 logging to stdout")
}

func getUpdatedPackages(ctx context.Context, svnRepoURL string, startRev int) ([]string, int, error) {
	revisionRange := fmt.Sprintf("%d:HEAD", startRev)
	cmd := exec.CommandContext(ctx, "svn", "log", "--xml", "-q", "-v", "-r", revisionRange, svnRepoURL)

	l := log.WithField("repo", svnRepoURL)
	l.Infof("fetching svn log for revision range %s", revisionRange)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "E160006: No such revision") {
			l.Info("no new revisions found.")
			return []string{}, startRev - 1, nil
		}
		return nil, 0, fmt.Errorf("svn log failed: %w\noutput: %s", err, string(output))
	}

	var svnLog SvnLog
	if err := xml.Unmarshal(output, &svnLog); err != nil {
		return nil, 0, fmt.Errorf("failed to parse svn log xml: %w", err)
	}

	if len(svnLog.Entries) == 0 {
		return []string{}, startRev - 1, nil
	}

	packageSet := make(map[string]struct{})
	newHeadRev := 0
	for _, entry := range svnLog.Entries {
		rev, _ := strconv.Atoi(entry.Revision)
		if rev > newHeadRev {
			newHeadRev = rev
		}
		for _, path := range entry.Paths {
			parts := strings.Split(strings.Trim(path.Path, "/"), "/")
			if len(parts) > 1 {
				packageSet[parts[0]] = struct{}{}
			}
		}
	}

	updatedPackages := make([]string, 0, len(packageSet))
	for pkg := range packageSet {
		updatedPackages = append(updatedPackages, pkg)
	}
	return updatedPackages, newHeadRev, nil
}

func getRemoteSvnTags(ctx context.Context, svnRepoURL, packageName, packageType string) ([]string, error) {
	var tagsSvnURL string
	if packageType == "plugin" {
		tagsSvnURL = fmt.Sprintf("%s/%s/tags", strings.TrimRight(svnRepoURL, "/"), packageName)
	} else {
		tagsSvnURL = fmt.Sprintf("%s/%s", strings.TrimRight(svnRepoURL, "/"), packageName)
	}

	cmd := exec.CommandContext(ctx, "svn", "list", tagsSvnURL)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "E170013") || strings.Contains(string(output), "non-existent") {
			return []string{}, nil
		}
		return nil, fmt.Errorf("svn list for %s failed: %w\noutput: %s", packageName, err, string(output))
	}

	var tags []string
	lines := strings.SplitSeq(string(output), "\n")
	for line := range lines {
		if trimmed := strings.Trim(line, "/ \r"); trimmed != "" {
			tags = append(tags, trimmed)
		}
	}
	return tags, nil
}

func getExistingVersions(ctx context.Context, registryURL, packageName string) (map[string]struct{}, error) {
	versions := make(map[string]struct{})
	url := fmt.Sprintf("https://%s/%s", registryURL, packageName)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for %s: %w", url, err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch versions from %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return versions, nil // No versions exist yet, return empty map
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status from registry %s: %s", url, resp.Status)
	}

	var registryResponse struct {
		Versions []struct {
			Version string `json:"version"`
		} `json:"versions"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&registryResponse); err != nil {
		return nil, fmt.Errorf("failed to decode registry response from %s: %w", url, err)
	}

	for _, v := range registryResponse.Versions {
		versions[v.Version] = struct{}{}
	}
	return versions, nil
}

func getLatestVersion(ctx context.Context, apiURL, packageName string) (string, error) {
	url := apiURL + packageName
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request for %s: %w", url, err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch latest version from %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("bad status from WP API %s: %s body: %s", url, resp.Status, string(bodyBytes))
	}

	var apiResponse struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResponse); err != nil {
		return "", fmt.Errorf("failed to decode WP API response from %s: %w", url, err)
	}

	return apiResponse.Version, nil
}

func checkoutTag(ctx context.Context, svnRepoURL, packageName, packageType, tag, workDir string) (string, error) {
	var packageSvnURL string
	localCheckoutPath := filepath.Join(workDir, packageName, tag)
	if err := os.MkdirAll(localCheckoutPath, 0755); err != nil {
		return "", fmt.Errorf("failed to create checkout directory %s: %w", localCheckoutPath, err)
	}

	if packageType == "plugin" {
		packageSvnURL = fmt.Sprintf("%s/%s/tags/%s", strings.TrimRight(svnRepoURL, "/"), packageName, tag)
	} else {
		packageSvnURL = fmt.Sprintf("%s/%s/%s", strings.TrimRight(svnRepoURL, "/"), packageName, tag)
	}

	cmd := exec.CommandContext(ctx, "svn", "export", packageSvnURL, localCheckoutPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(localCheckoutPath)
		return "", fmt.Errorf("svn export failed for %s@%s: %w\noutput: %s", packageName, tag, err, string(output))
	}
	return localCheckoutPath, nil
}

func runWpmCommand(ctx context.Context, wpmPath string, args []string, workDir string) error {
	cmd := exec.CommandContext(ctx, wpmPath, args...)
	cmd.Dir = workDir
	if output, err := cmd.CombinedOutput(); err != nil {
		log.WithFields(logrus.Fields{
			"cmd":     "wpm " + strings.Join(args, " "),
			"workDir": workDir,
			"output":  string(output),
		}).Error("❌ wpm command failed.")
		return fmt.Errorf("wpm command failed: %w", err)
	}
	return nil
}

func normalizeVersion(version string) (string, error) {
	if version == "" {
		return "", errors.New("version cannot be empty")
	}

	v, err := semver.NewVersion(version)
	if err == nil {
		return v.String(), nil
	}

	// Attempt to normalize the version format to be compatible with semver.
	// If version has more than 2 dots, we replace the last dot with a hyphen
	// Example:
	// 1.0.0.0 -> 1.0.0-0
	// 1.0.0.alpha.1+build -> 1.0.0-alpha.1+build
	parts := strings.Split(version, ".")
	if len(parts) > 3 {
		major := parts[0]
		minor := parts[1]
		patch := parts[2]
		prerelease := strings.Join(parts[3:], ".")

		version = fmt.Sprintf("%s.%s.%s-%s", major, minor, patch, prerelease)
	}

	// If version part start with 0, we remove it
	// Example:
	// 01.0.0 -> 1.0.0
	// 1.01.0 -> 1.1.0
	// 1.0.01 -> 1.0.1
	// 1.0.01-beta -> 1.0.1-beta
	// Split version into parts
	parts = strings.Split(version, ".")
	for i, part := range parts {
		// Check if part starts with '0' and has more characters
		if len(part) > 1 && part[0] == '0' {
			// Split part into numeric and non-numeric (e.g., "01-beta" -> "01" and "-beta")
			numericPart := part
			nonNumericPart := ""
			if hyphenIndex := strings.Index(part, "-"); hyphenIndex != -1 {
				numericPart = part[:hyphenIndex]
				nonNumericPart = part[hyphenIndex:]
			}

			// Check if numeric part is all digits and starts with '0'
			isNumeric := true
			for _, r := range numericPart {
				if !unicode.IsDigit(r) {
					isNumeric = false
					break
				}
			}

			if isNumeric && len(numericPart) > 1 && numericPart[0] == '0' {
				// Remove leading zeros from numeric part
				trimmed := strings.TrimLeft(numericPart, "0")
				if trimmed == "" {
					trimmed = "0"
				}
				// Reconstruct the part
				parts[i] = trimmed + nonNumericPart
			}
		}
	}
	version = strings.Join(parts, ".")

	v, err = semver.NewVersion(version)
	if err != nil {
		return "", err
	}

	return v.String(), nil
}

func processSinglePackage(
	ctx context.Context,
	packageName string,
	config *MigratorConfig,
) {
	l := log.WithField("package", packageName)
	l.Info("👷 worker started processing.")

	tags, err := getRemoteSvnTags(ctx, config.SvnRepo, packageName, config.PackageType)
	if err != nil {
		l.WithError(err).Error("❌ could not get svn tags.")
		return
	}
	if len(tags) == 0 {
		l.Info("✅ no tags found in svn repo.")
		return
	}
	l.Infof("found %d total tags in SVN.", len(tags))

	existingVersions, err := getExistingVersions(ctx, config.RegistryURL, packageName)
	if err != nil {
		l.WithError(err).Error("❌ could not get existing versions from registry.")
		return
	}

	latestVersion, err := getLatestVersion(ctx, config.WPApi, packageName)
	if err != nil {
		l.WithError(err).Warn("could not get latest version from wordpress.org API.")
	}
	l.Infof("found %d existing versions in registry. latest from wp.org is '%s'", len(existingVersions), latestVersion)

	for _, tag := range tags {
		tagLog := l.WithField("tag", tag)

		var normalizedTag string
		if normalizedTag, err = normalizeVersion(tag); err != nil {
			tagLog.WithError(err).Error("❌ failed to normalize tag version.")
			continue
		}

		if _, exists := existingVersions[normalizedTag]; exists {
			continue
		}
		tagLog.Info("🏷️ new tag found. starting migration.")

		func() {
			tagCtx, cancelTag := context.WithTimeout(ctx, config.TagTimeout)
			defer cancelTag()

			localPath, err := checkoutTag(tagCtx, config.SvnRepo, packageName, config.PackageType, tag, config.WorkDir)
			if err != nil {
				tagLog.WithError(err).Error("❌ tag checkout failed.")
				return
			}
			defer os.RemoveAll(localPath)

			initArgs := []string{"init", "--existing", "--name", packageName, "--version", tag, "--type", config.PackageType}
			if err := runWpmCommand(tagCtx, config.WpmPath, initArgs, localPath); err != nil {
				return
			}

			publishTagValue := "untagged"
			if tag == latestVersion {
				publishTagValue = "latest"
			}
			publishArgs := []string{"--registry", config.RegistryURL, "publish", "--access", "public", "--tag", publishTagValue}
			if err := runWpmCommand(tagCtx, config.WpmPath, publishArgs, localPath); err != nil {
				return
			}
			tagLog.Info("🎉 tag migrated successfully.")
		}()
	}
	l.Info("✅ worker finished processing.")
}

func migrationWorker(
	ctx context.Context,
	jobs <-chan string,
	wg *sync.WaitGroup,
	config *MigratorConfig,
) {
	defer wg.Done()
	for packageName := range jobs {
		processSinglePackage(ctx, packageName, config)
	}
}

type MigratorConfig struct {
	PackageType       string
	AllowedListFile   string
	RevisionStateFile string
	SvnRepo           string
	WPApi             string
	WorkDir           string
	WpmPath           string
	NumWorkers        int
	TagTimeout        time.Duration
	RegistryURL       string
}

func runMigrator(cmd *cobra.Command, args []string) error {
	setupLogger(cmd.Flags().Changed("verbose"))

	pkgType, _ := cmd.Flags().GetString("type")
	if pkgType != "plugin" && pkgType != "theme" {
		return fmt.Errorf("type must be 'plugin' or 'theme'")
	}

	wpmPath, err := exec.LookPath("wpm")
	if err != nil {
		return fmt.Errorf("wpm command not found in PATH and --wpm-path not specified")
	}

	config := &MigratorConfig{
		PackageType: pkgType,
		WpmPath:     wpmPath,
	}
	config.NumWorkers, _ = cmd.Flags().GetInt("workers")
	config.TagTimeout, _ = cmd.Flags().GetDuration("tag-timeout")
	config.RegistryURL, _ = cmd.Flags().GetString("registry")

	if pkgType == "plugin" {
		config.AllowedListFile = pluginsJSONFile
		config.RevisionStateFile = pluginRevFile
		config.SvnRepo = pluginRepo
		config.WPApi = pluginApi
	} else {
		config.AllowedListFile = themesJSONFile
		config.RevisionStateFile = themeRevFile
		config.SvnRepo = themeRepo
		config.WPApi = themeApi
	}

	tempDir, err := os.MkdirTemp("", "wpm-migration-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary working directory: %w", err)
	}
	config.WorkDir = tempDir
	log.Infof("📁 using temporary work directory: %s", tempDir)
	defer os.RemoveAll(config.WorkDir)

	lastRev, err := readLastRevision(config.RevisionStateFile)
	if err != nil {
		return err
	}
	log.Infof("last processed revision: %d", lastRev)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updatedPackages, newHeadRev, err := getUpdatedPackages(ctx, config.SvnRepo, lastRev+1)
	if err != nil {
		return fmt.Errorf("could not determine updated packages: %w", err)
	}

	if newHeadRev <= lastRev {
		log.Info("✅ repository is up-to-date. no new revisions found.")
		return nil
	}
	log.Infof("found %d packages with updates between revision %d and %d.", len(updatedPackages), lastRev, newHeadRev)

	allowedList, err := loadPackageList(config.AllowedListFile)
	if err != nil {
		return err
	}
	allowedSet := make(map[string]struct{}, len(allowedList))
	for _, pkg := range allowedList {
		allowedSet[pkg] = struct{}{}
	}

	packagesToProcess := make([]string, 0)
	for _, pkgName := range updatedPackages {
		if _, ok := allowedSet[pkgName]; ok {
			packagesToProcess = append(packagesToProcess, pkgName)
		}
	}

	if len(packagesToProcess) == 0 {
		log.Info("✅ no updates found for packages in the allowed list.")
	} else {
		log.Infof("found %d allowed packages to process: %v", len(packagesToProcess), packagesToProcess)
		jobs := make(chan string, len(packagesToProcess))
		var wg sync.WaitGroup
		for i := 0; i < config.NumWorkers; i++ {
			wg.Add(1)
			go migrationWorker(ctx, jobs, &wg, config)
		}
		for _, pkgName := range packagesToProcess {
			jobs <- pkgName
		}
		close(jobs)
		wg.Wait()
	}

	if err := writeLastRevision(config.RevisionStateFile, newHeadRev); err != nil {
		return fmt.Errorf("critical: failed to save final revision state %d: %w", newHeadRev, err)
	}
	log.Infof("📝 updated revision state to %d in %s.", newHeadRev, config.RevisionStateFile)

	log.Info("🎉 migration process complete!")
	return nil
}

func main() {
	rootCmd := &cobra.Command{
		Use:           "plugins-themes-migrator",
		Short:         "migrates new plugin/theme tags from svn to a wpm registry.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runMigrator,
	}

	rootCmd.Flags().StringP("type", "t", "", "type to migrate: 'plugin' or 'theme' (required)")
	rootCmd.Flags().IntP("workers", "w", defaultWorkers, "number of parallel migration workers")
	rootCmd.Flags().Duration("tag-timeout", defaultTagTimeout, "timeout for migrating a single tag")
	rootCmd.Flags().BoolP("verbose", "v", false, "enable verbose logging with full timestamps")
	rootCmd.Flags().StringP("registry", "r", "registry.wpm.so", "wpm registry url to publish to")
	_ = rootCmd.MarkFlagRequired("type")

	if err := rootCmd.Execute(); err != nil {
		log.Errorf("❌ %v", err)
		os.Exit(1)
	}
}
