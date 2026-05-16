package main

import (
	"context"
	"errors"
	"flag"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"wpm-migration/pkg/store"
	"wpm-migration/pkg/svn"
	"wpm-migration/pkg/wporg"

	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"
)

const progressEvery uint64 = 5000

func main() {
	zerolog.TimeFieldFormat = time.RFC3339
	logger := zerolog.New(zerolog.ConsoleWriter{
		Out:        os.Stderr,
		TimeFormat: time.DateTime,
	}).With().Timestamp().Logger()

	if _, err := exec.LookPath("svn"); err != nil {
		logger.Fatal().Err(err).Msg("svn command not found")
	}

	var workers int
	flag.IntVar(&workers, "w", 50, "Number of concurrent workers")
	flag.IntVar(&workers, "worker", 50, "Number of concurrent workers (alias)")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	runStart := time.Now()
	logger.Info().Int("workers", workers).Msg("Starting update")

	oldThemesList, err := store.GetThemes()
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to snapshot existing themes.json; aborting before backfill diff can over-trigger")
	}
	oldPluginsList, err := store.GetPlugins()
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to snapshot existing plugins.json; aborting before backfill diff can over-trigger")
	}
	oldThemes := make(map[string]struct{}, len(oldThemesList))
	for _, t := range oldThemesList {
		oldThemes[t] = struct{}{}
	}
	oldPlugins := make(map[string]struct{}, len(oldPluginsList))
	for _, p := range oldPluginsList {
		oldPlugins[p] = struct{}{}
	}

	svnStart := time.Now()
	logger.Info().Msg("Fetching SVN listings")

	eg, svnCtx := errgroup.WithContext(ctx)
	var themes, plugins map[string]struct{}

	eg.Go(func() error {
		var err error
		themes, err = svn.List(svnCtx, store.Theme)
		return err
	})
	eg.Go(func() error {
		var err error
		plugins, err = svn.List(svnCtx, store.Plugin)
		return err
	})

	if err := eg.Wait(); err != nil {
		logger.Fatal().Err(err).Msg("Failed to fetch SVN listings")
	}

	// wp.org hosts many thousands of themes and plugins. A listing that
	// returns under a thousand almost certainly means the upstream HTML
	// changed and our tokenizer is silently failing to extract entries —
	// refuse to overwrite the JSON files rather than mass-evict every
	// existing package as "not-whitelisted" on the next migrate run.
	if len(themes) < 1000 || len(plugins) < 1000 {
		logger.Fatal().
			Int("themes", len(themes)).
			Int("plugins", len(plugins)).
			Int("threshold", 1000).
			Msg("SVN listing returned suspiciously few entries; refusing to overwrite")
	}

	logger.Info().
		Int("themes", len(themes)).
		Int("plugins", len(plugins)).
		Dur("duration", time.Since(svnStart)).
		Msg("SVN listings fetched")

	var conflicts []string
	for theme := range themes {
		if _, exists := plugins[theme]; exists {
			conflicts = append(conflicts, theme)
		}
	}
	for _, conflict := range conflicts {
		delete(themes, conflict)
		delete(plugins, conflict)
	}

	resolved, err := store.GetResolved()
	if err != nil {
		logger.Warn().Err(err).Msg("Failed to read resolved.json; treating as empty")
	}

	resolvedThemes := make(map[string]struct{}, len(resolved.Themes))
	for _, t := range resolved.Themes {
		resolvedThemes[t] = struct{}{}
	}
	resolvedPlugins := make(map[string]struct{}, len(resolved.Plugins))
	for _, p := range resolved.Plugins {
		if _, dup := resolvedThemes[p]; dup {
			logger.Fatal().Str("slug", p).Msg("resolved.json lists slug under both themes and plugins")
		}
		resolvedPlugins[p] = struct{}{}
	}

	var reapplyAsTheme, reapplyAsPlugin int
	for _, conflict := range conflicts {
		if _, ok := resolvedThemes[conflict]; ok {
			themes[conflict] = struct{}{}
			reapplyAsTheme++
		} else if _, ok := resolvedPlugins[conflict]; ok {
			plugins[conflict] = struct{}{}
			reapplyAsPlugin++
		}
	}

	logger.Info().
		Int("conflicts", len(conflicts)).
		Int("reapplied_as_theme", reapplyAsTheme).
		Int("reapplied_as_plugin", reapplyAsPlugin).
		Int("unresolved", len(conflicts)-reapplyAsTheme-reapplyAsPlugin).
		Msg("Resolved theme/plugin conflicts")

	themesList := slices.Sorted(maps.Keys(themes))
	pluginsList := slices.Sorted(maps.Keys(plugins))
	slices.Sort(conflicts)

	if err := store.SetThemes(themesList); err != nil {
		logger.Fatal().Err(err).Msg("Failed to write themes.json")
	}
	if err := store.SetPlugins(pluginsList); err != nil {
		logger.Fatal().Err(err).Msg("Failed to write plugins.json")
	}
	if err := store.SetConflicts(conflicts); err != nil {
		logger.Fatal().Err(err).Msg("Failed to write conflicts.json")
	}

	logger.Info().
		Int("themes", len(themesList)).
		Int("plugins", len(pluginsList)).
		Int("conflicts", len(conflicts)).
		Msg("Wrote package lists")

	closedThemes, err := store.GetClosedThemes()
	if err != nil {
		logger.Warn().Err(err).Msg("Failed to read closed-themes.json; starting from empty")
	}
	closedPlugins, err := store.GetClosedPlugins()
	if err != nil {
		logger.Warn().Err(err).Msg("Failed to read closed-plugins.json; starting from empty")
	}

	existingClosedThemes := len(closedThemes)
	existingClosedPlugins := len(closedPlugins)

	themesToFetch := make([]string, 0, len(themesList))
	for _, t := range themesList {
		if _, ok := closedThemes[t]; !ok {
			themesToFetch = append(themesToFetch, t)
		}
	}
	pluginsToFetch := make([]string, 0, len(pluginsList))
	for _, p := range pluginsList {
		if _, ok := closedPlugins[p]; !ok {
			pluginsToFetch = append(pluginsToFetch, p)
		}
	}

	wpClient := wporg.New(wporg.WithConcurrency(workers * 2))

	var (
		themesMu, pluginsMu sync.Mutex
		fetchedCount        atomic.Uint64
		themeErrors         atomic.Uint64
		pluginErrors        atomic.Uint64
	)

	totalToFetch := len(themesToFetch) + len(pluginsToFetch)
	fetchStart := time.Now()
	logger.Info().
		Int("themes", len(themesToFetch)).
		Int("plugins", len(pluginsToFetch)).
		Int("total", totalToFetch).
		Int("concurrency", workers).
		Msg("Fetching package metadata")

	tickProgress := func() {
		if n := fetchedCount.Add(1); n%progressEvery == 0 {
			logger.Info().
				Uint64("fetched", n).
				Int("total", totalToFetch).
				Msg("Metadata progress")
		}
	}

	var updateEg errgroup.Group

	// Worker 1: Themes
	updateEg.Go(func() error {
		eg := new(errgroup.Group)
		eg.SetLimit(workers)
		for _, t := range themesToFetch {
			themeSlug := t
			eg.Go(func() error {
				if ctx.Err() != nil {
					return nil
				}
				defer tickProgress()

				info, err := wpClient.FetchThemeInfo(ctx, themeSlug)
				if err != nil {
					if errors.Is(err, wporg.ErrNotFound) {
						themesMu.Lock()
						closedThemes[themeSlug] = store.ClosureUnknown
						themesMu.Unlock()
					} else if ctx.Err() == nil {
						themeErrors.Add(1)
						logger.Error().
							Err(err).
							Str("type", "theme").
							Str("package", themeSlug).
							Str("step", "fetch-info").
							Msg("Package error")
					}
					return nil
				}

				if info != nil && strings.Contains(info.Error, "Theme not found") {
					themesMu.Lock()
					closedThemes[themeSlug] = store.ClosureUnknown
					themesMu.Unlock()
				}
				return nil
			})
		}
		return eg.Wait()
	})

	// Worker 2: Plugins
	updateEg.Go(func() error {
		eg := new(errgroup.Group)
		eg.SetLimit(workers)
		for _, p := range pluginsToFetch {
			pluginSlug := p
			eg.Go(func() error {
				if ctx.Err() != nil {
					return nil
				}
				defer tickProgress()

				info, err := wpClient.FetchPluginInfo(ctx, pluginSlug)
				if err != nil {
					if errors.Is(err, wporg.ErrNotFound) {
						pluginsMu.Lock()
						closedPlugins[pluginSlug] = store.ClosureUnknown
						pluginsMu.Unlock()
					} else if ctx.Err() == nil {
						pluginErrors.Add(1)
						logger.Error().
							Err(err).
							Str("type", "plugin").
							Str("package", pluginSlug).
							Str("step", "fetch-info").
							Msg("Package error")
					}
					return nil
				}

				if info != nil && info.Error != "" {
					closureType := store.ClosureUnknown
					if info.Error == "closed" {
						closureType = store.ClosureTemporary
						if strings.Contains(info.Description, "This closure is permanent.") {
							closureType = store.ClosurePermanent
						}
					}
					pluginsMu.Lock()
					closedPlugins[pluginSlug] = closureType
					pluginsMu.Unlock()
				}
				return nil
			})
		}
		return eg.Wait()
	})

	// Inner errgroups always return nil — per-package errors are logged in
	// place, not returned. updateEg.Wait is therefore guaranteed nil.
	_ = updateEg.Wait()

	logger.Info().
		Uint64("fetched", fetchedCount.Load()).
		Uint64("theme_errors", themeErrors.Load()).
		Uint64("plugin_errors", pluginErrors.Load()).
		Dur("duration", time.Since(fetchStart)).
		Msg("Package metadata fetched")

	if err := store.SetClosedThemes(closedThemes); err != nil {
		logger.Fatal().Err(err).Msg("Failed to write closed-themes.json")
	}
	if err := store.SetClosedPlugins(closedPlugins); err != nil {
		logger.Fatal().Err(err).Msg("Failed to write closed-plugins.json")
	}

	logger.Info().
		Int("closed_themes", len(closedThemes)).
		Int("closed_plugins", len(closedPlugins)).
		Int("added_themes", len(closedThemes)-existingClosedThemes).
		Int("added_plugins", len(closedPlugins)-existingClosedPlugins).
		Msg("Wrote closure lists")

	backfillThemes := make([]string, 0)
	for _, t := range themesList {
		if _, wasOld := oldThemes[t]; wasOld {
			continue
		}
		if _, isClosed := closedThemes[t]; isClosed {
			continue
		}
		backfillThemes = append(backfillThemes, t)
	}
	backfillPlugins := make([]string, 0)
	for _, p := range pluginsList {
		if _, wasOld := oldPlugins[p]; wasOld {
			continue
		}
		if _, isClosed := closedPlugins[p]; isClosed {
			continue
		}
		backfillPlugins = append(backfillPlugins, p)
	}

	if err := writePendingBackfill("pending-backfill-themes.txt", backfillThemes); err != nil {
		logger.Error().Err(err).Msg("Failed to write pending-backfill-themes.txt")
	}
	if err := writePendingBackfill("pending-backfill-plugins.txt", backfillPlugins); err != nil {
		logger.Error().Err(err).Msg("Failed to write pending-backfill-plugins.txt")
	}

	logger.Info().
		Int("themes", len(backfillThemes)).
		Int("plugins", len(backfillPlugins)).
		Msg("Wrote pending-backfill files")

	ev := logger.Info()
	msg := "Update finished"
	if ctx.Err() != nil {
		ev = logger.Warn()
		msg = "Update interrupted"
	}
	ev.
		Dur("duration", time.Since(runStart)).
		Int("themes", len(themesList)).
		Int("plugins", len(pluginsList)).
		Int("conflicts", len(conflicts)).
		Int("closed_themes", len(closedThemes)).
		Int("closed_plugins", len(closedPlugins)).
		Msg(msg)
}

// writePendingBackfill writes a newline-delimited list of slugs to path.
func writePendingBackfill(path string, slugs []string) error {
	var data []byte
	if len(slugs) > 0 {
		data = []byte(strings.Join(slugs, "\n") + "\n")
	}
	return os.WriteFile(path, data, 0644)
}
