package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
	"wpm-migration/pkg/store"
	"wpm-migration/pkg/svn"
	"wpm-migration/pkg/version"
	"wpm-migration/pkg/wporg"

	"github.com/newrelic/go-agent/v3/integrations/logcontext-v2/zerologWriter"
	"github.com/newrelic/go-agent/v3/newrelic"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
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
	url := "https://" + registry + "/" + slug
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for %s: %w", url, err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch versions from %s: %w", url, err)
	}
	defer resp.Body.Close()

	versions := make(map[string]struct{})

	if resp.StatusCode == http.StatusNotFound {
		return versions, nil // No versions exist yet, return empty map
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status from registry %s: %s", url, resp.Status)
	}

	var r struct {
		Versions []string `json:"versions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("failed to decode registry response from %s: %w", url, err)
	}

	for _, v := range r.Versions {
		versions[v] = struct{}{}
	}

	return versions, nil
}

func run(ctx context.Context, o Options, packages []string) error {
	pkgType := store.PackageType(o.migrationType)
	if !pkgType.Valid() {
		return fmt.Errorf("invalid migration type: %s", o.migrationType)
	}

	o.logger.Info().Str("type", o.migrationType).Int("count", len(packages)).Msg("Starting migration")

	whitelisted, err := store.GetPackages(pkgType)
	if err != nil {
		return fmt.Errorf("failed to get whitelisted %s: %w", pkgType, err)
	}

	closedPackages, err := store.GetClosedPackages(pkgType)
	if err != nil {
		return fmt.Errorf("failed to get closed %s: %w", pkgType, err)
	}

	wpClient := wporg.New(wporg.WithConcurrency(o.concurrency))

	for _, pkg := range packages {
		if ctx.Err() != nil {
			return nil
		}

		pkgLogger := o.logger.With().Str("package", pkg).Logger()

		pkgLogger.Info().Msgf("Migrating %s", pkgType)

		if _, ok := closedPackages[pkg]; ok {
			pkgLogger.Warn().Msgf("Skipping closed %s", pkgType)
			continue // Skip closed packages
		}

		if !slices.Contains(whitelisted, pkg) {
			pkgLogger.Warn().Msgf("Skipping non-whitelisted %s", pkgType)
			continue // Skip packages not in the whitelist
		}

		info, err := wpClient.FetchPackageInfo(ctx, pkgType, pkg)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return nil
			}
			pkgLogger.Error().Err(err).Msgf("Failed to fetch info for %s", pkgType)
			continue
		}

		if info.Error != "" && info.Version == "" {
			pkgLogger.Error().Str("error", info.Error).Msgf("Skipping closed %s with no version info", pkgType)
			continue
		}

		publishedVersions, err := getPublishedVersions(ctx, o.registry, pkg)
		if err != nil {
			pkgLogger.Error().Err(err).Msg("Failed to fetch published versions")
			continue
		}

		tags, err := svn.ListTags(ctx, pkgType, pkg)
		if err != nil {
			pkgLogger.Error().Err(err).Msg("Failed to list SVN tags")
			continue
		}

		type pendingTag struct {
			raw        string
			normalized string
		}

		tagsToMigrate := make([]pendingTag, 0, len(tags))
		for tag := range tags {
			normalized, err := version.Normalize(tag)
			if err != nil {
				continue // Skip tags that can't be normalized as versions
			}

			if _, exists := publishedVersions[normalized]; !exists {
				tagsToMigrate = append(tagsToMigrate, pendingTag{raw: tag, normalized: normalized})
			}
		}

		if len(tagsToMigrate) == 0 {
			pkgLogger.Info().Msg("No new versions to migrate")
			continue
		}

		latestRaw := string(info.Version)

		var eg errgroup.Group
		eg.SetLimit(o.concurrency)

		for _, pt := range tagsToMigrate {
			if ctx.Err() != nil {
				break
			}

			pt := pt

			eg.Go(func() error {
				if ctx.Err() != nil {
					return nil
				}

				tagCtx, cancel := context.WithTimeout(ctx, o.tagTimeout)
				defer cancel()

				tagLogger := pkgLogger.With().
					Str("tag", pt.raw).
					Str("version", pt.normalized).
					Logger()

				tagLogger.Info().Msg("Migrating tag")

				exportPath, cleanup, err := svn.Export(tagCtx, pkgType, pkg, pt.raw)
				if err != nil {
					if ctx.Err() == nil {
						tagLogger.Error().Err(err).Msg("svn export failed")
					}
					return nil
				}
				defer cleanup()

				if err := wpmExec(tagCtx, exportPath,
					"init", "--existing",
					"--name", pkg,
					"--version", pt.normalized,
					"--type", string(pkgType),
				); err != nil {
					if ctx.Err() == nil {
						tagLogger.Error().Err(err).Msg("wpm init failed")
					}
					return nil
				}

				distTag := "untagged"
				if latestRaw != "" && latestRaw == pt.raw {
					distTag = "latest"
				}

				if err := wpmExec(tagCtx, exportPath,
					"--registry", o.registry,
					"publish",
					"--access", "public",
					"--tag", distTag,
				); err != nil {
					if ctx.Err() == nil {
						tagLogger.Error().Err(err).Msg("wpm publish failed")
					}
					return nil
				}

				tagLogger.Info().Str("dist_tag", distTag).Msg("Tag migrated")
				return nil
			})
		}

		if err := eg.Wait(); err != nil {
			if ctx.Err() != nil {
				pkgLogger.Warn().Msg("Migration interrupted")
				return nil
			}

			pkgLogger.Error().Err(err).Msg("Failed to migrate all tags")
			continue
		}
	}

	return nil
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

			var headRev int
			if len(args) == 0 {
				args, headRev, err = svn.GetUpdatedPackages(cmd.Context(), pkgType, rev+1)
				if err != nil {
					return fmt.Errorf("failed to get updated packages: %w", err)
				}
			}

			// bail if still no packages to migrate after fetching updates
			if len(args) == 0 {
				opts.logger.Info().
					Int("last_revision", rev).
					Int("head_revision", headRev).
					Msg("No new packages found to migrate")
				return nil
			}

			if err := run(cmd.Context(), opts, args); err != nil {
				return fmt.Errorf("migration failed: %w", err)
			}

			if cmd.Context().Err() != nil {
				opts.logger.Warn().Msg("Migration interrupted, exiting...")
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
