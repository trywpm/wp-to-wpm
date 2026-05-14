package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"wpm-migration/pkg/wporg"

	"golang.org/x/net/html"
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

var (
	hrefBytes   = []byte("href")
	parentBytes = []byte("../")
	httpClient  = &http.Client{}
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

func isValidPackageName(name []byte) bool {
	n := len(name)
	if n < 3 || n > 164 {
		return false
	}

	for i := range n {
		c := name[i]

		// check for allowed characters a-z
		if c >= 'a' && c <= 'z' {
			continue
		}

		// check for allowed characters 0-9
		if c >= '0' && c <= '9' {
			continue
		}

		// check for allowed special characters `-`
		if c == '-' {
			if i == 0 || i == n-1 || name[i-1] == '-' {
				return false
			}
			continue
		}

		return false
	}

	return true
}

func getSvnList(ctx context.Context, svnRepo string) (map[string]struct{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, svnRepo, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch data from %s: %w", svnRepo, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch repo %s, status code: %d", svnRepo, resp.StatusCode)
	}

	list := make(map[string]struct{})
	z := html.NewTokenizer(resp.Body)

	for {
		switch z.Next() {
		case html.ErrorToken:
			err := z.Err()
			if errors.Is(err, io.EOF) {
				return list, nil
			}
			return nil, fmt.Errorf("error tokenizing html from %s: %w", svnRepo, err)

		case html.StartTagToken:
			name, hasAttr := z.TagName()
			if !hasAttr || len(name) != 1 || name[0] != 'a' {
				continue
			}

			for {
				k, v, more := z.TagAttr()
				if bytes.Equal(k, hrefBytes) {
					if len(v) > 1 && v[len(v)-1] == '/' && !bytes.Equal(v, parentBytes) {
						slug := v[:len(v)-1]
						if isValidPackageName(slug) {
							list[string(slug)] = struct{}{}
						}
					}
					break
				}
				if !more {
					break
				}
			}
		}
	}
}

func readJson(filename string, dest any) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("failed to read file %s: %w", filename, err)
	}

	if len(data) == 0 {
		return nil
	}

	if err := json.Unmarshal(data, dest); err != nil {
		return fmt.Errorf("failed to unmarshal json from file %s: %w", filename, err)
	}

	return nil
}

func writeJson(path string, data any) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*.json")
	if err != nil {
		return fmt.Errorf("failed to create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(data); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to encode json to %s: %w", tmpName, err)
	}

	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to fsync %s: %w", tmpName, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close %s: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("failed to rename %s -> %s: %w", tmpName, path, err)
	}

	return nil
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
		themes, err = getSvnList(svnCtx, themesSvnRepo)
		return err
	})
	eg.Go(func() error {
		var err error
		plugins, err = getSvnList(svnCtx, pluginsSvnRepo)
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
	if err := readJson(resolvedJson, &resolved); err != nil {
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

	if err := writeJson(themesJson, themesList); err != nil {
		log.Fatalf("failed to write themes json: %v", err)
	}
	if err := writeJson(pluginsJson, pluginsList); err != nil {
		log.Fatalf("failed to write plugins json: %v", err)
	}
	if err := writeJson(conflictsJson, conflicts); err != nil {
		log.Fatalf("failed to write conflicts json: %v", err)
	}

	log.Println("successfully updated themes, plugins and conflicts packages data.")

	closedThemes := make(map[string]packageClosure)
	closedPlugins := make(map[string]packageClosure)

	if err := readJson(closedThemesJson, &closedThemes); err != nil {
		log.Printf("warning: failed to read closed themes json: %v", err)
	}
	if err := readJson(closedPluginsJson, &closedPlugins); err != nil {
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

	if err := writeJson(closedThemesJson, closedThemes); err != nil {
		log.Fatalf("failed to write closed themes json: %v", err)
	}
	if err := writeJson(closedPluginsJson, closedPlugins); err != nil {
		log.Fatalf("failed to write closed plugins json: %v", err)
	}

	log.Println("successfully updated closed packages data")

	log.Printf("\nthemes=%d plugins=%d conflicts=%d (resolved=%d) closed-themes=%d closed-plugins=%d",
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
