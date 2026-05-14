package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"wpm-migration/pkg/store"
	"wpm-migration/pkg/svn"
	"wpm-migration/pkg/validate"
	"wpm-migration/pkg/wporg"

	"golang.org/x/sync/errgroup"
)

const (
	// output files.
	themesJson        = "themes.json"
	pluginsJson       = "plugins.json"
	resolvedJson      = "resolved.json"
	conflictsJson     = "conflicts.json"
	closedThemesJson  = "closed-themes.json"
	closedPluginsJson = "closed-plugins.json"

	// svn repos.
	themesSvnRepo  = "https://themes.svn.wordpress.org/"
	pluginsSvnRepo = "https://plugins.svn.wordpress.org/"
)

type packageClosure string

const (
	closureUnknown   packageClosure = "unknown"
	closureTemporary packageClosure = "temporary"
	closurePermanent packageClosure = "permanent"
)

type resolvedConfig struct {
	Themes  []string `json:"themes"`
	Plugins []string `json:"plugins"`
}

func main() {
	var workers int
	flag.IntVar(&workers, "w", 50, "Number of concurrent workers")
	flag.IntVar(&workers, "worker", 50, "Number of concurrent workers (alias)")
	flag.Parse()

	ctx := context.Background()

	log.Printf("fetching themes and plugins list from svn with %d workers...", workers)

	eg, svnCtx := errgroup.WithContext(ctx)

	var themes, plugins map[string]struct{}

	eg.Go(func() error {
		var err error
		themes, err = svn.List(svnCtx, themesSvnRepo, validate.PackageName)
		return err
	})
	eg.Go(func() error {
		var err error
		plugins, err = svn.List(svnCtx, pluginsSvnRepo, validate.PackageName)
		return err
	})

	if err := eg.Wait(); err != nil {
		log.Fatalf("failed to fetch data: %v", err)
	}

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

	var resolved resolvedConfig
	if err := store.GetData(store.Resolved, &resolved); err != nil {
		log.Fatalf("failed to read resolved config: %v", err)
	}

	resolvedThemes := make(map[string]struct{}, len(resolved.Themes))
	for _, t := range resolved.Themes {
		resolvedThemes[t] = struct{}{}
	}
	for _, p := range resolved.Plugins {
		if _, dup := resolvedThemes[p]; dup {
			log.Fatalf("resolved.json: %q is listed under both themes and plugins", p)
		}
	}

	for _, conflict := range conflicts {
		if _, ok := resolvedThemes[conflict]; ok {
			themes[conflict] = struct{}{}
		} else if slices.Contains(resolved.Plugins, conflict) {
			plugins[conflict] = struct{}{}
		}
	}

	themesList := slices.Sorted(maps.Keys(themes))
	pluginsList := slices.Sorted(maps.Keys(plugins))
	slices.Sort(conflicts)

	if err := store.SetData(store.Themes, themesList); err != nil {
		log.Fatalf("failed to write themes json: %v", err)
	}
	if err := store.SetData(store.Plugins, pluginsList); err != nil {
		log.Fatalf("failed to write plugins json: %v", err)
	}
	if err := store.SetData(store.Conflicts, conflicts); err != nil {
		log.Fatalf("failed to write conflicts json: %v", err)
	}

	log.Println("successfully updated themes, plugins and conflicts packages data.")

	closedThemes := make(map[string]packageClosure)
	closedPlugins := make(map[string]packageClosure)

	if err := store.GetData(store.ClosedThemes, &closedThemes); err != nil {
		log.Printf("warning: failed to read closed themes json: %v", err)
	}
	if err := store.GetData(store.ClosedPlugins, &closedPlugins); err != nil {
		log.Printf("warning: failed to read closed plugins json: %v", err)
	}

	var themesToFetch []string
	for _, t := range themesList {
		if _, ok := closedThemes[t]; !ok {
			themesToFetch = append(themesToFetch, t)
		}
	}

	var pluginsToFetch []string
	for _, p := range pluginsList {
		if _, ok := closedPlugins[p]; !ok {
			pluginsToFetch = append(pluginsToFetch, p)
		}
	}

	wpClient := wporg.New(wporg.WithConcurrency(workers * 2))

	var updateEg errgroup.Group
	var fetchedCount atomic.Uint32
	var themesMu, pluginsMu sync.Mutex

	log.Println("fetching info for themes and plugins to determine closures...")

	// Worker 1: Updating Themes
	updateEg.Go(func() error {
		egThemes := new(errgroup.Group)
		egThemes.SetLimit(workers)

		for _, t := range themesToFetch {
			themeSlug := t

			egThemes.Go(func() error {
				defer func() {
					if fetchedCount.Add(1)%1000 == 0 {
						fmt.Print(".")
						os.Stdout.Sync()
					}
				}()

				info, err := wpClient.FetchThemeInfo(ctx, themeSlug)
				if err != nil {
					if errors.Is(err, wporg.ErrNotFound) {
						themesMu.Lock()
						closedThemes[themeSlug] = closureUnknown
						themesMu.Unlock()
					} else {
						// Don't fail the entire process, just log temporary failures.
						log.Printf("failed to fetch info for theme %s: %v", themeSlug, err)
					}
					return nil
				}

				if info != nil && strings.Contains(info.Error, "Theme not found") {
					themesMu.Lock()
					closedThemes[themeSlug] = closureUnknown
					themesMu.Unlock()
				}
				return nil
			})
		}

		return egThemes.Wait()
	})

	// Worker 2: Updating Plugins
	updateEg.Go(func() error {
		egPlugins := new(errgroup.Group)
		egPlugins.SetLimit(workers)

		for _, p := range pluginsToFetch {
			pluginSlug := p

			egPlugins.Go(func() error {
				defer func() {
					if fetchedCount.Add(1)%1000 == 0 {
						fmt.Print(".")
						os.Stdout.Sync()
					}
				}()

				info, err := wpClient.FetchPluginInfo(ctx, pluginSlug)
				if err != nil {
					if errors.Is(err, wporg.ErrNotFound) {
						pluginsMu.Lock()
						closedPlugins[pluginSlug] = closureUnknown
						pluginsMu.Unlock()
					} else {
						// Don't fail the entire process, just log temporary failures.
						log.Printf("failed to fetch info for plugin %s: %v", pluginSlug, err)
					}
					return nil
				}

				if info != nil && info.Error != "" {
					closureType := closureUnknown

					if info.Error == "closed" {
						closureType = closureTemporary

						if strings.Contains(info.Description, "This closure is permanent.") {
							closureType = closurePermanent
						}
					}

					pluginsMu.Lock()
					closedPlugins[pluginSlug] = closureType
					pluginsMu.Unlock()
				}
				return nil
			})
		}

		return egPlugins.Wait()
	})

	if err := updateEg.Wait(); err != nil {
		fmt.Println()
		log.Printf("workers finished with some errors: %v", err)
	} else {
		fmt.Println()
	}

	if err := store.SetData(store.ClosedThemes, closedThemes); err != nil {
		log.Fatalf("failed to write closed themes json: %v", err)
	}
	if err := store.SetData(store.ClosedPlugins, closedPlugins); err != nil {
		log.Fatalf("failed to write closed plugins json: %v", err)
	}

	log.Println("successfully updated closed packages data")

	log.Printf("themes=%d plugins=%d conflicts=%d (resolved=%d) closed-themes=%d closed-plugins=%d",
		len(themesList), len(pluginsList), len(conflicts),
		intersectCount(conflicts, resolvedThemes, resolved.Plugins),
		len(closedThemes), len(closedPlugins))
}

func intersectCount(conflicts []string, resolvedThemes map[string]struct{}, resolvedPlugins []string) int {
	resolvedPluginsSet := make(map[string]struct{}, len(resolvedPlugins))
	for _, p := range resolvedPlugins {
		resolvedPluginsSet[p] = struct{}{}
	}
	n := 0
	for _, c := range conflicts {
		if _, ok := resolvedThemes[c]; ok {
			n++
			continue
		}
		if _, ok := resolvedPluginsSet[c]; ok {
			n++
		}
	}
	return n
}
