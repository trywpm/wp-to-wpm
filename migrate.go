package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/pkg/errors"
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
)

var (
	log        = logrus.New()
	nameReg    = regexp.MustCompile(`^[\w-]{3,164}$`)
	httpClient = &http.Client{Timeout: 30 * time.Second}
)

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

func checkoutPackage(ctx context.Context, svnRepoURL, packageName, packageType, workDir string) (string, error) {
	var packageSvnURL string
	localCheckoutPath := filepath.Join(workDir, packageName)

	if err := os.MkdirAll(localCheckoutPath, 0755); err != nil {
		return "", fmt.Errorf("failed to create checkout directory %s: %w", localCheckoutPath, err)
	}

	if packageType == "plugin" {
		packageSvnURL = fmt.Sprintf("%s/%s/tags", strings.TrimRight(svnRepoURL, "/"), packageName)
	} else {
		packageSvnURL = fmt.Sprintf("%s/%s", strings.TrimRight(svnRepoURL, "/"), packageName)
	}

	cmd := exec.CommandContext(ctx, "svn", "checkout", packageSvnURL, localCheckoutPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(localCheckoutPath)
		return "", fmt.Errorf("svn checkout failed for %s: %w\noutput: %s", packageName, err, string(output))
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
	parts = strings.Split(version, ".")
	for i, part := range parts {
		if len(part) > 1 && part[0] == '0' {
			// Remove leading zero
			parts[i] = strings.TrimLeft(part, "0")
			if parts[i] == "" {
				// If the part becomes empty, set it to "0"
				parts[i] = "0"
			}
		}
	}
	version = strings.Join(parts, ".")

	v, err := semver.NewVersion(version)
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

	// Check if package name is valid
	if !nameReg.MatchString(packageName) {
		l.Error("❌ name is not wpm registry compliant")
		return
	}

	// First check if package is qualified by getting latest version from WordPress API
	latestVersion, err := getLatestVersion(ctx, config.WPApi, packageName)
	if err != nil {
		l.WithError(err).Error("❌ package not qualified - could not get latest version from wordpress.org API.")
		return
	}
	l.Infof("✅ package qualified - latest version from wp.org is '%s'", latestVersion)

	packagePath, err := checkoutPackage(ctx, config.SvnRepo, packageName, config.PackageType, config.WorkDir)
	if err != nil {
		l.WithError(err).Error("❌ could not checkout package from svn.")
		return
	}
	defer os.RemoveAll(packagePath)

	// Loop over directories in the checked out path
	entries, err := os.ReadDir(packagePath)
	if err != nil {
		l.WithError(err).Error("❌ could not read package directory.")
		return
	}

	tagCount := 0
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == ".svn" {
			continue
		}

		tagCount++
		tag := entry.Name()
		tagPath := filepath.Join(packagePath, tag)

		tagLog := l.WithField("tag", tag)

		if _, err = normalizeVersion(tag); err != nil {
			tagLog.WithError(err).Error("❌ failed to normalize tag version.")
			continue
		}

		tagLog.Info("🏷️ processing tag for migration.")

		func() {
			tagCtx, cancelTag := context.WithTimeout(ctx, config.TagTimeout)
			defer cancelTag()

			initArgs := []string{"init", "--existing", "--name", packageName, "--version", tag, "--type", config.PackageType}
			if err := runWpmCommand(tagCtx, config.WpmPath, initArgs, tagPath); err != nil {
				return
			}

			publishTagValue := "untagged"
			if tag == latestVersion {
				publishTagValue = "latest"
			}
			publishArgs := []string{"--registry", config.RegistryURL, "publish", "--access", "public", "--tag", publishTagValue}
			if err := runWpmCommand(tagCtx, config.WpmPath, publishArgs, tagPath); err != nil {
				return
			}
			tagLog.Info("🎉 tag migrated successfully.")
		}()
	}

	if tagCount == 0 {
		l.Info("⚠️ no tags found in package.")
	} else {
		l.Infof("found %d total tags.", tagCount)
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
	PackageType string
	SvnRepo     string
	WPApi       string
	WorkDir     string
	WpmPath     string
	NumWorkers  int
	TagTimeout  time.Duration
	RegistryURL string
}

func runMigrator(cmd *cobra.Command, args []string) error {
	setupLogger(cmd.Flags().Changed("verbose"))

	pkgType, _ := cmd.Flags().GetString("type")
	if pkgType != "plugin" && pkgType != "theme" {
		return fmt.Errorf("type must be 'plugin' or 'theme'")
	}

	packageListFile, _ := cmd.Flags().GetString("package-list")
	if packageListFile == "" {
		return fmt.Errorf("package-list flag is required - please provide a JSON file containing array of package names")
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

	workDir, _ := cmd.Flags().GetString("work-dir")
	if workDir == "" {
		tempDir, err := os.MkdirTemp("", "wpm-migration-*")
		if err != nil {
			return fmt.Errorf("failed to create temporary working directory: %w", err)
		}
		config.WorkDir = tempDir
		log.Infof("📁 using temporary work directory: %s", tempDir)
		defer os.RemoveAll(config.WorkDir)
	} else {
		if err := os.MkdirAll(workDir, 0755); err != nil {
			return fmt.Errorf("failed to create work directory %s: %w", workDir, err)
		}
		config.WorkDir = workDir
		log.Infof("📁 using work directory: %s", workDir)
	}

	// Set SVN repo and API URLs based on package type
	if pkgType == "plugin" {
		config.SvnRepo = pluginRepo
		config.WPApi = pluginApi
	} else {
		config.SvnRepo = themeRepo
		config.WPApi = themeApi
	}

	// Load package list from file
	packageList, err := loadPackageList(packageListFile)
	if err != nil {
		return fmt.Errorf("failed to load package list: %w", err)
	}

	if len(packageList) == 0 {
		return fmt.Errorf("package list is empty - please provide a non-empty JSON array of package names")
	}

	log.Infof("loaded %d packages from %s: %v", len(packageList), packageListFile, packageList)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start workers
	jobs := make(chan string, len(packageList))
	var wg sync.WaitGroup
	for i := 0; i < config.NumWorkers; i++ {
		wg.Add(1)
		go migrationWorker(ctx, jobs, &wg, config)
	}

	// Send packages to workers
	for _, pkgName := range packageList {
		jobs <- pkgName
	}
	close(jobs)

	// Wait for all workers to complete
	wg.Wait()

	log.Info("🎉 initial migration process complete!")
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
	rootCmd.Flags().StringP("package-list", "p", "", "path to JSON file containing array of package names to migrate (required)")
	rootCmd.Flags().StringP("work-dir", "d", "", "directory to use for package checkouts (defaults to /tmp/wpm-migration-*)")
	rootCmd.Flags().IntP("workers", "w", defaultWorkers, "number of parallel migration workers")
	rootCmd.Flags().Duration("tag-timeout", defaultTagTimeout, "timeout for migrating a single tag")
	rootCmd.Flags().BoolP("verbose", "v", false, "enable verbose logging with full timestamps")
	rootCmd.Flags().StringP("registry", "r", "registry.wpm.so", "wpm registry url to publish to")

	_ = rootCmd.MarkFlagRequired("type")
	_ = rootCmd.MarkFlagRequired("package-list")

	if err := rootCmd.Execute(); err != nil {
		log.Errorf("❌ %v", err)
		os.Exit(1)
	}
}
