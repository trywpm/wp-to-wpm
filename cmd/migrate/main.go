package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"wpm-migration/pkg/store"
	"wpm-migration/pkg/svn"
	"wpm-migration/pkg/wporg"

	"github.com/newrelic/go-agent/v3/integrations/logcontext-v2/zerologWriter"
	"github.com/newrelic/go-agent/v3/newrelic"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
	"go.wpm.so/cli/pkg/version"
	"golang.org/x/sync/errgroup"
)

var (
	httpClient = &http.Client{Timeout: 30 * time.Second}
)

type Options struct {
	registry      string
	migrationType string
	concurrency   int
	tagTimeout    time.Duration
	logger        *zerolog.Logger
}

// wpmExec executes a wpm command with the given args in the specified working directory.
func wpmExec(ctx context.Context, cwd string, args ...string) error {
	cmd := exec.CommandContext(ctx, "wpm", args...)
	cmd.Dir = cwd

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("wpm command failed: %w, stderr: %s", err, strings.TrimSpace(stderr.String()))
	}

	return nil
}

// getPublishedVersions fetches the list of published versions for a given package from the wpm registry.
func getPublishedVersions(ctx context.Context, registry, slug string) (map[string]struct{}, error) {
	base := registry
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	url := base + "/" + slug
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for %s: %w", url, err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch versions from %s: %w", url, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusNotFound {
		return map[string]struct{}{}, nil // No versions exist yet, return empty map
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status from registry %s: %s", url, resp.Status)
	}

	var r struct {
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("failed to decode registry response from %s: %w", url, err)
	}

	versions := make(map[string]struct{}, len(r.Versions))
	for v := range r.Versions {
		versions[v] = struct{}{}
	}

	return versions, nil
}

type counters struct {
	migrated      atomic.Int64
	upToDate      atomic.Int64
	skipClosed    atomic.Int64
	skipWhitelist atomic.Int64
	skipNoInfo    atomic.Int64
	errored       atomic.Int64
	tagsPublished atomic.Int64
	tagsFailed    atomic.Int64
	distTagSet    atomic.Int64
	distTagFailed atomic.Int64
}

func run(ctx context.Context, o Options, packages []string) error {
	pkgType := store.PackageType(o.migrationType)
	if !pkgType.Valid() {
		return fmt.Errorf("invalid migration type: %s", o.migrationType)
	}

	whitelistedList, err := store.GetPackages(pkgType)
	if err != nil {
		return fmt.Errorf("failed to get whitelisted %s: %w", pkgType, err)
	}
	whitelisted := make(map[string]struct{}, len(whitelistedList))
	for _, w := range whitelistedList {
		whitelisted[w] = struct{}{}
	}

	closedPackages, err := store.GetClosedPackages(pkgType)
	if err != nil {
		return fmt.Errorf("failed to get closed %s: %w", pkgType, err)
	}

	wpClient := wporg.New(wporg.WithConcurrency(o.concurrency))

	start := time.Now()

	var c counters

	defer func() {
		ev := o.logger.Info()
		msg := "Migration finished"
		if ctx.Err() != nil {
			ev = o.logger.Warn()
			msg = "Migration interrupted"
		}
		ev.
			Dur("duration", time.Since(start)).
			Int("packages", len(packages)).
			Int64("migrated", c.migrated.Load()).
			Int64("up_to_date", c.upToDate.Load()).
			Int64("skipped_closed", c.skipClosed.Load()).
			Int64("skipped_not_whitelisted", c.skipWhitelist.Load()).
			Int64("skipped_no_info", c.skipNoInfo.Load()).
			Int64("errored", c.errored.Load()).
			Int64("tags_published", c.tagsPublished.Load()).
			Int64("tags_failed", c.tagsFailed.Load()).
			Int64("dist_tags_set", c.distTagSet.Load()).
			Int64("dist_tags_failed", c.distTagFailed.Load()).
			Msg(msg)
	}()

	// Packages run concurrently. Tags within a package are published
	// sequentially because the registry locks a per-package object on publish.
	var eg errgroup.Group
	eg.SetLimit(o.concurrency)

	for _, pkg := range packages {
		if ctx.Err() != nil {
			break
		}

		pkg := pkg
		eg.Go(func() error {
			migratePackage(ctx, o, pkgType, pkg, wpClient, closedPackages, whitelisted, &c)
			return nil
		})
	}

	_ = eg.Wait()

	return nil
}

func migratePackage(
	ctx context.Context,
	o Options,
	pkgType store.PackageType,
	pkg string,
	wpClient *wporg.Client,
	closedPackages map[string]store.PackageClosure,
	whitelisted map[string]struct{},
	c *counters,
) {
	pkgLogger := o.logger.With().Str("package", pkg).Logger()

	if _, ok := closedPackages[pkg]; ok {
		pkgLogger.Info().Str("reason", "closed").Msg("Skipping package")
		c.skipClosed.Add(1)
		return
	}

	if _, ok := whitelisted[pkg]; !ok {
		pkgLogger.Info().Str("reason", "not-whitelisted").Msg("Skipping package")
		c.skipWhitelist.Add(1)
		return
	}

	info, err := wpClient.FetchPackageInfo(ctx, pkgType, pkg)
	if err != nil {
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return
		}
		pkgLogger.Error().Err(err).Str("step", "fetch-info").Msg("Package error")
		c.errored.Add(1)
		return
	}

	if info.Error != "" && info.Version == "" {
		pkgLogger.Info().
			Str("reason", "no-version-info").
			Str("api_error", info.Error).
			Msg("Skipping package")
		c.skipNoInfo.Add(1)
		return
	}

	publishedVersions, err := getPublishedVersions(ctx, o.registry, pkg)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		pkgLogger.Error().Err(err).Str("step", "fetch-published-versions").Msg("Package error")
		c.errored.Add(1)
		return
	}

	tags, err := svn.ListTags(ctx, pkgType, pkg)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		pkgLogger.Error().Err(err).Str("step", "list-svn-tags").Msg("Package error")
		c.errored.Add(1)
		return
	}

	var latestNormalized string
	if latestRaw := string(info.Version); latestRaw != "" {
		if n, err := version.Normalize(latestRaw); err == nil {
			latestNormalized = n
		}
	}

	type pendingTag struct {
		raw        string
		normalized string
	}

	// Hold the stable version aside so it is published last. Publishing with no
	// --tag makes that version latest, so a stable-last order sets latest
	// correctly without a dist-tag call.
	tagsToMigrate := make([]pendingTag, 0, len(tags))
	var stable pendingTag
	haveStable := false
	for tag := range tags {
		normalized, err := version.Normalize(tag)
		if err != nil {
			continue
		}

		if _, published := publishedVersions[normalized]; published {
			continue
		}

		if latestNormalized != "" && normalized == latestNormalized {
			stable = pendingTag{raw: tag, normalized: normalized}
			haveStable = true
			continue
		}

		tagsToMigrate = append(tagsToMigrate, pendingTag{raw: tag, normalized: normalized})
	}
	if haveStable {
		tagsToMigrate = append(tagsToMigrate, stable)
	}

	if len(tagsToMigrate) == 0 {
		pkgLogger.Info().
			Int("svn_tags", len(tags)).
			Int("published", len(publishedVersions)).
			Msg("Package up-to-date")
		c.upToDate.Add(1)
		return
	}

	pkgStart := time.Now()
	pkgLogger.Info().
		Int("svn_tags", len(tags)).
		Int("published", len(publishedVersions)).
		Int("to_migrate", len(tagsToMigrate)).
		Msg("Migrating package")

	var ok, fail int
	var latest string
	for _, pt := range tagsToMigrate {
		if ctx.Err() != nil {
			break
		}

		if publishTag(ctx, o, pkgType, pkg, pt.raw, pt.normalized, &pkgLogger) {
			ok++
			c.tagsPublished.Add(1)
			publishedVersions[pt.normalized] = struct{}{}
			latest = pt.normalized
		} else if ctx.Err() == nil {
			fail++
			c.tagsFailed.Add(1)
		}
	}

	if ok > 0 || fail > 0 {
		c.migrated.Add(1)
	}

	// A publish leaves latest on the last published version. If the stable
	// version is published but did not land last, point latest back at it. This
	// is the only case that spends a dist-tag call.
	if ctx.Err() == nil && latest != "" && latestNormalized != "" && latest != latestNormalized {
		if _, published := publishedVersions[latestNormalized]; published {
			setLatestTag(ctx, o, pkg, latestNormalized, &pkgLogger, c)
		} else {
			pkgLogger.Warn().
				Str("stable", latestNormalized).
				Str("latest", latest).
				Msg("Stable version not published; leaving latest unchanged")
		}
	}

	if ctx.Err() != nil {
		return
	}

	pkgLogger.Info().
		Int("migrated", ok).
		Int("failed", fail).
		Dur("duration", time.Since(pkgStart)).
		Msg("Package finished")
}

func publishTag(ctx context.Context, o Options, pkgType store.PackageType, pkg, rawTag, normalized string, pkgLogger *zerolog.Logger) bool {
	tagCtx, cancel := context.WithTimeout(ctx, o.tagTimeout)
	defer cancel()

	tagLogger := pkgLogger.With().
		Str("tag", rawTag).
		Str("version", normalized).
		Logger()

	tagStart := time.Now()
	tagLogger.Info().Msg("Migrating tag")

	exportPath, cleanup, err := svn.Export(tagCtx, pkgType, pkg, rawTag)
	if err != nil {
		if ctx.Err() == nil {
			tagLogger.Error().Err(err).Str("step", "svn-export").Msg("Tag failed")
		}
		return false
	}
	defer cleanup()

	if err := wpmExec(tagCtx, exportPath,
		"init", "--existing",
		"--name", pkg,
		"--version", normalized,
		"--type", string(pkgType),
	); err != nil {
		if ctx.Err() == nil {
			tagLogger.Error().Err(err).Str("step", "wpm-init").Msg("Tag failed")
		}
		return false
	}

	if err := wpmExec(tagCtx, exportPath,
		"--registry", o.registry,
		"publish",
		"--access", "public",
	); err != nil {
		if ctx.Err() == nil {
			tagLogger.Error().Err(err).Str("step", "wpm-publish").Msg("Tag failed")
		}
		return false
	}

	tagLogger.Info().
		Dur("duration", time.Since(tagStart)).
		Msg("Tag migrated")
	return true
}

func setLatestTag(ctx context.Context, o Options, pkg, normalized string, pkgLogger *zerolog.Logger, c *counters) {
	tagCtx, cancel := context.WithTimeout(ctx, o.tagTimeout)
	defer cancel()

	if err := wpmExec(tagCtx, ".",
		"--registry", o.registry,
		"dist-tag", "add",
		pkg+"@"+normalized,
	); err != nil {
		if ctx.Err() == nil {
			pkgLogger.Error().Err(err).
				Str("step", "wpm-dist-tag").
				Str("version", normalized).
				Msg("Failed to set latest dist-tag")
			c.distTagFailed.Add(1)
		}
		return
	}

	pkgLogger.Info().Str("version", normalized).Msg("Set latest dist-tag")
	c.distTagSet.Add(1)
}

func main() {
	var opts Options

	cmd := &cobra.Command{
		Use:           "migrate",
		Short:         "Migrate plugins and themes from wp.org to wpm",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := exec.LookPath("svn")
			if err != nil {
				return fmt.Errorf("svn command not found: %w", err)
			}

			_, err = exec.LookPath("wpm")
			if err != nil {
				return fmt.Errorf("wpm command not found: %w", err)
			}

			pkgType := store.PackageType(opts.migrationType)
			if !pkgType.Valid() {
				return fmt.Errorf("invalid migration type: %s", opts.migrationType)
			}

			rev, err := store.GetLastSvnRevision(pkgType)
			if err != nil {
				return fmt.Errorf("failed to get last SVN revision: %w", err)
			}

			// Zero rev pointer means the state file is missing, empty,
			// or literally "0".
			if rev == 0 && len(args) == 0 {
				return fmt.Errorf("refusing to scan svn from rev 1: .%s_last_rev is missing or 0. Initialize it to a recent revision, or invoke with explicit slugs to bootstrap", pkgType)
			}

			var headRev int
			if len(args) == 0 {
				args, headRev, err = svn.GetUpdatedPackages(cmd.Context(), pkgType, rev+1)
				if err != nil {
					if errors.Is(err, context.Canceled) || cmd.Context().Err() != nil {
						return nil
					}
					return fmt.Errorf("failed to get updated packages: %w", err)
				}
			}

			// A single migrate run should never process thousands of
			// packages.
			const maxPackagesPerRun = 1000
			if len(args) > maxPackagesPerRun {
				return fmt.Errorf("svn log returned %d packages (safety cap %d). Advance .%s_last_rev closer to HEAD and re-run, or invoke with a smaller explicit slug list", len(args), maxPackagesPerRun, pkgType)
			}

			// bail if still no packages to migrate after fetching updates
			if len(args) == 0 {
				opts.logger.Info().
					Int("last_revision", rev).
					Int("head_revision", headRev).
					Msg("No new packages found to migrate")

				// still advance the SVN revision pointer to avoid repeatedly fetching the same updates on next run.
				if headRev > rev {
					if err := store.SetLastSvnRevision(pkgType, headRev); err != nil {
						return fmt.Errorf("failed to update last SVN revision: %w", err)
					}
				}
				return nil
			}

			opts.logger.Info().
				Str("type", opts.migrationType).
				Int("last_revision", rev).
				Int("head_revision", headRev).
				Int("packages", len(args)).
				Int("concurrency", opts.concurrency).
				Dur("tag_timeout", opts.tagTimeout).
				Msg("Starting migration")

			if err := run(cmd.Context(), opts, args); err != nil {
				return fmt.Errorf("migration failed: %w", err)
			}

			// don't advance the SVN revision pointer if the run was interrupted
			if cmd.Context().Err() != nil {
				return nil
			}

			if headRev > rev {
				if err := store.SetLastSvnRevision(store.PackageType(opts.migrationType), headRev); err != nil {
					return fmt.Errorf("failed to update last SVN revision: %w", err)
				}
			}

			return nil
		},
	}

	cmd.Flags().IntVarP(&opts.concurrency, "concurrency", "c", 2, "Number of concurrent migrations")
	cmd.Flags().DurationVar(&opts.tagTimeout, "tag-timeout", 8*time.Minute, "Timeout for migrating a single tag")

	cmd.Flags().StringVarP(&opts.registry, "registry", "r", "registry.wpm.so", "wpm registry url")
	cmd.Flags().StringVarP(&opts.migrationType, "type", "t", "", "Type of migration (plugin or theme)")

	if err := cmd.MarkFlagRequired("type"); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	app, err := newrelic.NewApplication(
		newrelic.ConfigAppName("wp-to-wpm migrate"),
		newrelic.ConfigFromEnvironment(),
		newrelic.ConfigAppLogForwardingEnabled(true),
		newrelic.ConfigEnabled(os.Getenv("CI") == "true"),
	)
	if err != nil {
		panic(fmt.Sprintf("failed to create New Relic application: %v", err))
	}

	app.WaitForConnection(5 * time.Second)
	defer app.Shutdown(10 * time.Second)

	zerolog.TimeFieldFormat = time.RFC3339

	consoleWriter := zerolog.ConsoleWriter{
		Out:        os.Stderr,
		TimeFormat: time.DateTime,
	}
	nrWriter := zerologWriter.New(consoleWriter, app)

	logger := zerolog.New(nrWriter).With().Timestamp().Logger()
	opts.logger = &logger

	if err := cmd.ExecuteContext(ctx); err != nil {
		logger.Error().Err(err).Msg("Migration failed")
		app.Shutdown(10 * time.Second)
		os.Exit(1)
	}
}
