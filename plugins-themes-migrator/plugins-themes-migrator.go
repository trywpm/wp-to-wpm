package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

const (
	defaultWorkers          = 5
	defaultTagTimeout       = 5 * time.Minute
	defaultLogFile          = "migrator-activity.log"
	pluginRepoURL           = "https://plugins.svn.wordpress.org"
	themeRepoURL            = "https://themes.svn.wordpress.org"
	pluginsJSONFile         = "plugins.json"
	themesJSONFile          = "themes.json"
	migratedPluginsJSONFile = "migrated-plugins.json"
	migratedThemesJSONFile  = "migrated-themes.json"
)

var log = logrus.New()

func loadPackageMap(path string) (map[string]string, error) {
	packages := make(map[string]string)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return packages, nil // file doesn't exist, return empty map
		}
		return nil, fmt.Errorf("failed to read package map %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &packages); err != nil {
		return nil, fmt.Errorf("failed to parse package map %s: %w", path, err)
	}
	return packages, nil
}

func savePackageMap(path string, packages map[string]string) error {
	data, err := json.MarshalIndent(packages, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal package map: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// FileHook enables dual logging: structured JSON to file, readable text to stdout
type FileHook struct {
	file      *os.File
	formatter logrus.Formatter
	levels    []logrus.Level
}

func NewFileHook(filePath string, formatter logrus.Formatter, levels []logrus.Level) (*FileHook, error) {
	logDir := filepath.Dir(filePath)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory %s: %w", logDir, err)
	}

	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file %s: %w", filePath, err)
	}

	return &FileHook{file: file, formatter: formatter, levels: levels}, nil
}

func (hook *FileHook) Fire(entry *logrus.Entry) error {
	lineBytes, err := hook.formatter.Format(entry)
	if err != nil {
		return err
	}
	_, err = hook.file.Write(lineBytes)
	return err
}

func (hook *FileHook) Levels() []logrus.Level {
	if len(hook.levels) == 0 {
		return logrus.AllLevels
	}
	return hook.levels
}

func setupLogger(logLevel logrus.Level, jsonLogFilePath string, verbose bool) error {
	log.SetOutput(os.Stdout)
	log.SetLevel(logLevel)

	if verbose {
		log.SetFormatter(&logrus.TextFormatter{
			FullTimestamp:   true,
			TimestampFormat: "2006-01-02 15:04:05.000",
			ForceColors:     true,
		})
	} else {
		log.SetFormatter(&logrus.TextFormatter{
			TimestampFormat: "15:04:05",
			ForceColors:     true,
		})
	}

	if jsonLogFilePath != "" {
		fileHook, err := NewFileHook(jsonLogFilePath, &logrus.JSONFormatter{
			TimestampFormat: time.RFC3339Nano,
		}, logrus.AllLevels)

		if err != nil {
			log.WithError(err).Errorf("❌ failed to initialize file logging: %s", jsonLogFilePath)
		} else {
			log.AddHook(fileHook)
			log.Infof("📝 logging: text to stdout, json to %s", jsonLogFilePath)
		}
	} else {
		log.Info("📝 logging: text to stdout only")
	}

	return nil
}

func checkoutPackage(ctx context.Context, svnRepoURL, packageName, packageType, outputDir string) (string, error) {
	var packageSvnURL string
	localCheckoutPath := filepath.Join(outputDir, packageName)

	if packageType == "plugin" {
		packageSvnURL = fmt.Sprintf("%s/%s/tags", strings.TrimRight(svnRepoURL, "/"), packageName)
	} else {
		packageSvnURL = fmt.Sprintf("%s/%s", strings.TrimRight(svnRepoURL, "/"), packageName)
	}

	_ = os.RemoveAll(localCheckoutPath)
	cmd := exec.CommandContext(ctx, "svn", "checkout", packageSvnURL, localCheckoutPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("svn checkout failed for %s: %w\noutput: %s", packageName, err, string(output))
	}
	return localCheckoutPath, nil
}

func getPackageSvnTags(tagsPath string) ([]string, error) {
	entries, err := os.ReadDir(tagsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("failed to read tags directory %s: %w", tagsPath, err)
	}
	var tags []string
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != ".svn" {
			tags = append(tags, entry.Name())
		}
	}
	return tags, nil
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

func removeDirectoryWithRetry(path string, retries int, delay time.Duration) {
	var err error
	for i := range retries {
		err = os.RemoveAll(path)
		if err == nil {
			log.WithField("path", path).Debug("Successfully removed temporary directory.")
			return
		}
		log.WithFields(logrus.Fields{
			"path":    path,
			"attempt": i + 1,
			"error":   err,
		}).Warn("🧹 Failed to remove temporary directory, retrying...")
		time.Sleep(delay)
	}
	log.WithFields(logrus.Fields{
		"path":  path,
		"error": err,
	}).Error("❌ Failed to remove temporary directory after all retries.")
}

func processSinglePackage(
	ctx context.Context,
	packageName string,
	config *MigratorConfig,
	packagesToProcess map[string]string,
	successfulMigrations map[string]string,
	successMu *sync.Mutex,
) {
	l := log.WithField("package", packageName)
	l.Info("👷 worker started processing.")

	localPath, err := checkoutPackage(ctx, config.SvnRepoURL, packageName, config.PackageType, config.WorkDir)
	if err != nil {
		l.WithError(err).Error("❌ checkout failed.")
		return
	}
	defer removeDirectoryWithRetry(localPath, 5, 200*time.Millisecond)

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

		tagCtx, cancelTag := context.WithTimeout(ctx, config.TagTimeout)
		defer cancelTag()

		initArgs := []string{"init", "--migrate", "--name", packageName, "--version", tag, "--type", config.PackageType}
		if err := runWpmCommand(tagCtx, config.WpmPath, initArgs, tagPath); err != nil {
			continue
		}

		publishTagValue := "untagged"
		latestVersion, hasLatest := packagesToProcess[packageName]
		if hasLatest && tag == latestVersion {
			publishTagValue = "latest"
		}

		publishArgs := []string{"--registry", config.RegistryURL, "publish", "--access", "public", "--tag", publishTagValue}
		if err := runWpmCommand(tagCtx, config.WpmPath, publishArgs, tagPath); err != nil {
			continue
		}
		l.WithField("tag", tag).Info("🎉 tag migrated successfully.")

		if publishTagValue == "latest" {
			successMu.Lock()
			successfulMigrations[packageName] = latestVersion
			successMu.Unlock()
			l.Infof("✅ successfully migrated to latest version %s.", latestVersion)
		}
	}

	l.Info("✅ worker finished processing.")
}

func migrationWorker(
	ctx context.Context,
	jobs <-chan string,
	wg *sync.WaitGroup,
	config *MigratorConfig,
	packagesToProcess map[string]string,
	successfulMigrations map[string]string,
	successMu *sync.Mutex,
) {
	defer wg.Done()
	for packageName := range jobs {
		processSinglePackage(ctx, packageName, config, packagesToProcess, successfulMigrations, successMu)
	}
}

type MigratorConfig struct {
	PackageType      string
	DesiredStateFile string
	CurrentStateFile string
	SvnRepoURL       string
	WorkDir          string
	WpmPath          string
	NumWorkers       int
	TagTimeout       time.Duration
	RegistryURL      string
}

func runMigrator(cmd *cobra.Command, args []string) error {
	logFilePath, _ := cmd.Flags().GetString("log-file")
	if err := setupLogger(logrus.InfoLevel, logFilePath, cmd.Flags().Changed("verbose")); err != nil {
		fmt.Fprintf(os.Stderr, "critical: failed to setup logger: %v\n", err)
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
		config.DesiredStateFile = pluginsJSONFile
		config.CurrentStateFile = migratedPluginsJSONFile
		config.SvnRepoURL = pluginRepoURL
	} else {
		config.DesiredStateFile = themesJSONFile
		config.CurrentStateFile = migratedThemesJSONFile
		config.SvnRepoURL = themeRepoURL
	}

	workDir, err := os.MkdirTemp("", "wpm-migration-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary working directory: %w", err)
	}
	config.WorkDir = workDir
	defer os.RemoveAll(workDir)
	log.Infof("📁 using temporary work directory: %s", workDir)

	desiredState, err := loadPackageMap(config.DesiredStateFile)
	if err != nil {
		return err
	}
	currentState, err := loadPackageMap(config.CurrentStateFile)
	if err != nil {
		return err
	}

	packagesToProcess := make(map[string]string)
	for pkgName, desiredVersion := range desiredState {
		if currentVersion, ok := currentState[pkgName]; !ok || currentVersion != desiredVersion {
			packagesToProcess[pkgName] = desiredVersion
		}
	}

	if len(packagesToProcess) == 0 {
		log.Info("✅ all packages are up-to-date. no migration needed.")
		return nil
	}

	log.Infof("found %d packages to migrate (new or updated).", len(packagesToProcess))

	successfulMigrations := make(map[string]string)
	var successMu sync.Mutex

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jobs := make(chan string, len(packagesToProcess))
	var wg sync.WaitGroup

	for range config.NumWorkers {
		wg.Add(1)
		go migrationWorker(ctx, jobs, &wg, config, packagesToProcess, successfulMigrations, &successMu)
	}

	for pkgName := range packagesToProcess {
		jobs <- pkgName
	}
	close(jobs)

	wg.Wait()

	if len(successfulMigrations) > 0 {
		log.Infof("updating migrated state file with %d successful migrations...", len(successfulMigrations))
		maps.Copy(currentState, successfulMigrations)
		if err := savePackageMap(config.CurrentStateFile, currentState); err != nil {
			log.WithError(err).Error("❌ failed to save updated migrated state file.")
		} else {
			log.Infof("✅ successfully saved migrated state to %s.", config.CurrentStateFile)
		}
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
	rootCmd.Flags().StringP("registry", "r", "registry.wpm.so", "registry url to use for wpm commands")
	_ = rootCmd.MarkFlagRequired("type")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "❌ error: %v\n", err)
		os.Exit(1)
	}
}
