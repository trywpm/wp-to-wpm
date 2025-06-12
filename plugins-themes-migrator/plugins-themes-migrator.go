// FILE: plugins-themes-migrator.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"slices"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

const (
	defaultWorkers      = 5
	defaultTagTimeout   = 5 * time.Minute
	defaultLogFile      = "migrator-activity.log"
	pluginRepoURL       = "https://plugins.svn.wordpress.org"
	themeRepoURL        = "https://themes.svn.wordpress.org"
	pluginsJSONFile     = "plugins.json"
	themesJSONFile      = "themes.json"
	pluginsManifestFile = "plugins-manifest.json"
	themesManifestFile  = "themes-manifest.json"
)

var log = logrus.New()

// Manifest stores the migration state for packages.
// It holds a map of package names to their failed tags.
// This allows us to track which tags have already failed migration attempts.
type Manifest struct {
	mu       sync.Mutex
	path     string
	Packages map[string][]string `json:"packages"`
}

func (m *Manifest) Load(path string) error {
	m.path = path
	m.Packages = make(map[string][]string)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // file doesn't exist, which is fine.
		}
		return fmt.Errorf("failed to read manifest %s: %w", path, err)
	}

	return json.Unmarshal(data, &m.Packages)
}

func (m *Manifest) Save() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, err := json.MarshalIndent(m.Packages, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}
	return os.WriteFile(m.path, data, 0644)
}

// AddFailure safely adds a failed tag to the manifest.
func (m *Manifest) AddFailure(packageName, tag string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// avoid duplicates
	if slices.Contains(m.Packages[packageName], tag) {
		return
	}

	m.Packages[packageName] = append(m.Packages[packageName], tag)
}

func setupLogger(logFilePath string) error {
	log.SetLevel(logrus.InfoLevel)
	log.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
		ForceColors:   true,
	})

	file, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return fmt.Errorf("failed to open log file %s: %w", logFilePath, err)
	}

	log.SetOutput(io.MultiWriter(os.Stdout, file))
	log.Infof("logging to stdout and %s", logFilePath)

	return nil
}

// checkoutPackage performs an SVN checkout of the specified package.
// It returns the local path where the package was checked out.
func checkoutPackage(ctx context.Context, svnRepoURL, packageName, packageType, outputDir string) (string, error) {
	var packageSvnURL string
	localCheckoutPath := filepath.Join(outputDir, packageName)

	// for plugins, we only want the 'tags' directory to save space and time.
	// for themes, we need the whole directory as there's no standard 'tags' subdir.
	if packageType == "plugin" {
		packageSvnURL = fmt.Sprintf("%s/%s/tags", strings.TrimRight(svnRepoURL, "/"), packageName)
	} else {
		packageSvnURL = fmt.Sprintf("%s/%s", strings.TrimRight(svnRepoURL, "/"), packageName)
	}

	// clean up any previous checkout to avoid conflicts.
	_ = os.RemoveAll(localCheckoutPath)

	cmd := exec.CommandContext(ctx, "svn", "checkout", packageSvnURL, localCheckoutPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("svn checkout failed for %s: %w\noutput: %s", packageName, err, string(output))
	}

	return localCheckoutPath, nil
}

// getPackageSvnTags reads the local directory structure to find version tags.
func getPackageSvnTags(tagsPath string) ([]string, error) {
	entries, err := os.ReadDir(tagsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil // no tags directory is not a fatal error.
		}
		return nil, fmt.Errorf("failed to read tags directory %s: %w", tagsPath, err)
	}

	var tags []string
	for _, entry := range entries {
		if entry.IsDir() {
			tags = append(tags, entry.Name())
		}
	}
	return tags, nil
}

// runWpmCommand executes wpm init or wpm publish.
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

// processSinglePackage processes a single package by checking it out, finding tags, and migrating each tag.
// It handles the logic for initializing and publishing with wpm, including error handling and logging.
func processSinglePackage(
	ctx context.Context,
	packageName string,
	config *MigratorConfig,
	manifest *Manifest,
	packagesToMigrate map[string]string,
) {
	l := log.WithField("package", packageName)
	l.Info("👷 worker started processing.")

	localPath, err := checkoutPackage(ctx, config.SvnRepoURL, packageName, config.PackageType, config.WorkDir)
	if err == nil {
		defer os.RemoveAll(localPath)
	}

	if err != nil {
		if err.Error() == "no tags directory" {
			return
		}

		l.WithError(err).Error("❌ checkout failed.")

		return
	}

	tagsPath := localPath
	tags, err := getPackageSvnTags(tagsPath)
	if err != nil {
		l.WithError(err).Error("❌ could not get svn tags.")
		return
	}
	if len(tags) == 0 {
		l.Info("✅ no tags found to migrate.")
		return
	}
	l.Infof("found %d tags to process.", len(tags))

	for _, tag := range tags {
		tagPath := filepath.Join(tagsPath, tag)
		l.WithField("tag", tag).Info("🏷️ migrating tag.")

		isFailed := false
		manifest.mu.Lock()
		if slices.Contains(manifest.Packages[packageName], tag) {
			isFailed = true
		}
		manifest.mu.Unlock()
		if isFailed {
			l.WithField("tag", tag).Warn("⏭️ skipping previously failed tag.")
			continue
		}

		tagCtx, cancelTag := context.WithTimeout(ctx, config.TagTimeout)

		// wpm init
		initArgs := []string{"init", "--migrate", "--name", packageName, "--version", tag}
		err = runWpmCommand(tagCtx, config.WpmPath, initArgs, tagPath)
		if err != nil {
			manifest.AddFailure(packageName, tag)
			cancelTag()
			continue // try next tag
		}

		// find correct tag to publish
		publishTagValue := "untagged"
		latestVersion, hasLatest := packagesToMigrate[packageName]
		if hasLatest && tag == latestVersion {
			publishTagValue = "latest"
		}
		l.Infof("publishing with --tag %s", publishTagValue)

		// wpm publish
		publishArgs := []string{"--registry", config.RegistryURL, "publish", "--access", "public", "--tag", publishTagValue}
		err = runWpmCommand(tagCtx, config.WpmPath, publishArgs, tagPath)
		if err != nil {
			manifest.AddFailure(packageName, tag)
			cancelTag()
			continue // try next tag
		}
		cancelTag()
		l.WithField("tag", tag).Info("🎉 tag migrated successfully.")
	}

	l.Info("✅ worker finished processing.")
}

// migrationWorker now just pulls from the job channel and calls the processing function.
func migrationWorker(
	ctx context.Context,
	jobs <-chan string,
	wg *sync.WaitGroup,
	config *MigratorConfig,
	manifest *Manifest,
	packagesToMigrate map[string]string,
) {
	defer wg.Done()
	for packageName := range jobs {
		processSinglePackage(ctx, packageName, config, manifest, packagesToMigrate)
	}
}

type MigratorConfig struct {
	PackageType  string
	InputFile    string
	ManifestFile string
	SvnRepoURL   string
	WorkDir      string
	WpmPath      string
	NumWorkers   int
	TagTimeout   time.Duration
	RegistryURL  string
}

func runMigrator(cmd *cobra.Command, args []string) error {
	logFilePath, _ := cmd.Flags().GetString("log-file")
	if err := setupLogger(logFilePath); err != nil {
		return err
	}

	pkgType, _ := cmd.Flags().GetString("type")
	if pkgType != "plugin" && pkgType != "theme" {
		return fmt.Errorf("type must be 'plugin' or 'theme'")
	}

	wpmPath, _ := cmd.Flags().GetString("wpm-path")
	if wpmPath == "" {
		var err error
		wpmPath, err = exec.LookPath("wpm")
		if err != nil {
			return fmt.Errorf("wpm command not found in path and --wpm-path not specified")
		}
	}

	config := &MigratorConfig{
		PackageType: pkgType,
		WpmPath:     wpmPath,
	}
	config.NumWorkers, _ = cmd.Flags().GetInt("workers")
	config.TagTimeout, _ = cmd.Flags().GetDuration("tag-timeout")
	config.RegistryURL, _ = cmd.Flags().GetString("registry")

	if pkgType == "plugin" {
		config.InputFile = pluginsJSONFile
		config.ManifestFile = pluginsManifestFile
		config.SvnRepoURL = pluginRepoURL
	} else {
		config.InputFile = themesJSONFile
		config.ManifestFile = themesManifestFile
		config.SvnRepoURL = themeRepoURL
	}

	workDir, err := os.MkdirTemp("", "wpm-migration-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary working directory: %w", err)
	}
	config.WorkDir = workDir
	defer os.RemoveAll(workDir)
	log.Infof("📁 using temporary work directory: %s", workDir)

	// load plugins or themes to migrate from the existing JSON file
	packagesToMigrate := make(map[string]string)
	data, err := os.ReadFile(config.InputFile)
	if err != nil {
		return fmt.Errorf("input file %s not found. please run the generator first: %w", config.InputFile, err)
	}
	if err := json.Unmarshal(data, &packagesToMigrate); err != nil {
		return fmt.Errorf("failed to parse %s: %w", config.InputFile, err)
	}

	manifest := &Manifest{}
	if err := manifest.Load(config.ManifestFile); err != nil {
		return fmt.Errorf("could not load manifest %s: %w", config.ManifestFile, err)
	}

	log.Infof("🚀 starting migration of %d %ss with %d workers.", len(packagesToMigrate), pkgType, config.NumWorkers)

	// fire up workers to process the migration
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jobs := make(chan string, len(packagesToMigrate))
	var wg sync.WaitGroup

	for range config.NumWorkers {
		wg.Add(1)
		go migrationWorker(ctx, jobs, &wg, config, manifest, packagesToMigrate)
	}

	for pkgName := range packagesToMigrate {
		jobs <- pkgName
	}
	close(jobs)

	wg.Wait()

	log.Info("💾 saving final manifest...")
	if err := manifest.Save(); err != nil {
		log.WithError(err).Error("❌ failed to save final manifest.")
		return err
	}

	log.Info("🎉 migration process complete!")
	return nil
}

func main() {
	rootCmd := &cobra.Command{
		Use:           "plugins-themes-migrator",
		Short:         "migrates plugins or themes from svn to wpm using a json input file.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runMigrator,
	}

	rootCmd.Flags().StringP("type", "t", "", "type to migrate: 'plugin' or 'theme' (required)")
	rootCmd.Flags().IntP("workers", "w", defaultWorkers, "number of parallel migration workers")
	rootCmd.Flags().Duration("tag-timeout", defaultTagTimeout, "timeout for migrating a single tag")
	rootCmd.Flags().String("wpm-path", "", "path to wpm binary (if not in path)")
	rootCmd.Flags().String("log-file", defaultLogFile, "path to the activity log file")
	rootCmd.Flags().StringP("registry", "r", "registry.wpm.so", "registry URL to use for wpm commands")
	_ = rootCmd.MarkFlagRequired("type")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "❌ error: %v\n", err)
		os.Exit(1)
	}
}
