package svn

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"wpm-migration/pkg/store"
	"wpm-migration/pkg/unsafeconv"
	"wpm-migration/pkg/validate"
	"wpm-migration/pkg/version"

	"golang.org/x/net/html"
)

const (
	themesSvnRepo  = "https://themes.svn.wordpress.org"
	pluginsSvnRepo = "https://plugins.svn.wordpress.org"
)

var (
	httpClient = &http.Client{}
)

var (
	hrefBytes   = []byte("href")
	parentBytes = []byte("../")
)

func List(ctx context.Context, pkgType store.PackageType) (map[string]struct{}, error) {
	var svnRepo string
	if pkgType == store.Theme {
		svnRepo = themesSvnRepo
	} else {
		svnRepo = pluginsSvnRepo
	}

	return list(ctx, svnRepo, validate.PackageName)
}

// ListPluginTags lists all tags for a given plugin slug.
//
// Items are returned as a map where the key is the original tag name
// from SVN and the value is the normalized version string.
func ListPluginTags(ctx context.Context, pluginSlug string) (map[string]string, error) {
	if !validate.PackageName(unsafeconv.StringToBytes(pluginSlug)) {
		return nil, fmt.Errorf("invalid plugin slug: %s", pluginSlug)
	}

	// svn plugin repo stores tags under /tags/ subdirectory.
	svnRepo := fmt.Sprintf("%s/%s/tags/", pluginsSvnRepo, pluginSlug)

	items, err := list(ctx, svnRepo, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list plugin tags: %w", err)
	}

	validatedItems := make(map[string]string, len(items))
	for item := range items {
		v, err := version.Normalize(item)
		if err != nil {
			continue
		}

		validatedItems[item] = v
	}

	return validatedItems, nil
}

// ListThemeTags lists all tags for a given theme slug.
//
// Items are returned as a map where the key is the original tag name
// from SVN and the value is the normalized version string.
func ListThemeTags(ctx context.Context, themeSlug string) (map[string]string, error) {
	if !validate.PackageName(unsafeconv.StringToBytes(themeSlug)) {
		return nil, fmt.Errorf("invalid theme slug: %s", themeSlug)
	}

	svnRepo := fmt.Sprintf("%s/%s", themesSvnRepo, themeSlug)

	items, err := list(ctx, svnRepo, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list theme tags: %w", err)
	}

	validatedItems := make(map[string]string, len(items))
	for item := range items {
		v, err := version.Normalize(item)
		if err != nil {
			continue
		}

		validatedItems[item] = v
	}

	return validatedItems, nil
}

func list(ctx context.Context, svnRepo string, isValid func([]byte) bool) (map[string]struct{}, error) {
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

						if isValid != nil && !isValid(slug) {
							continue
						}

						list[string(slug)] = struct{}{}
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

type SvnLogEntry struct {
	Revision string    `xml:"revision,attr"`
	Paths    []SvnPath `xml:"paths>path"`
}

type SvnPath struct {
	Path string `xml:",chardata"`
}

func GetUpdatedPackages(ctx context.Context, pkgType store.PackageType, startRev int) ([]string, int, error) {
	if startRev <= 0 {
		return nil, 0, fmt.Errorf("invalid start revision: %d", startRev)
	}

	var svnRepoURL string
	if pkgType == store.Theme {
		svnRepoURL = themesSvnRepo
	} else {
		svnRepoURL = pluginsSvnRepo
	}

	revisionRange := fmt.Sprintf("%d:HEAD", startRev)

	cmd := exec.CommandContext(ctx, "svn", "log", "--xml", "-q", "-v", "--non-interactive", "-r", revisionRange, svnRepoURL)

	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, 0, fmt.Errorf("failed to start svn command: %w", err)
	}

	packageSet := make(map[string]struct{})
	newHeadRev := 0

	decoder := xml.NewDecoder(stdout)
	for {
		t, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}

		// Look for <logentry> tags
		switch se := t.(type) {
		case xml.StartElement:
			if se.Name.Local == "logentry" {
				var entry SvnLogEntry
				if err := decoder.DecodeElement(&entry, &se); err != nil {
					return nil, 0, fmt.Errorf("failed to decode svn log entry: %w", err)
				}

				rev, err := strconv.Atoi(entry.Revision)
				if err != nil {
					return nil, 0, fmt.Errorf("failed to parse revision %q: %w", entry.Revision, err)
				}

				if rev > newHeadRev {
					newHeadRev = rev
				}

				for _, p := range entry.Paths {
					parts := strings.Split(strings.Trim(p.Path, "/"), "/")
					if len(parts) > 1 {
						packageSet[parts[0]] = struct{}{}
					}
				}
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		errMsg := stderrBuf.String()

		if strings.Contains(errMsg, "E160006") {
			return []string{}, startRev - 1, nil
		}

		return nil, 0, fmt.Errorf("svn log failed: %w\nstderr: %s", err, errMsg)
	}

	if newHeadRev == 0 {
		return []string{}, startRev - 1, nil
	}

	updatedPackages := make([]string, 0, len(packageSet))
	for pkg := range packageSet {
		if !validate.PackageName(unsafeconv.StringToBytes(pkg)) {
			continue
		}

		updatedPackages = append(updatedPackages, pkg)
	}

	sort.Strings(updatedPackages)
	return updatedPackages, newHeadRev, nil
}

func UpdatedPlugins(ctx context.Context, startRev int) ([]string, int, error) {
	return GetUpdatedPackages(ctx, store.Plugin, startRev)
}

func UpdatedThemes(ctx context.Context, startRev int) ([]string, int, error) {
	return GetUpdatedPackages(ctx, store.Theme, startRev)
}
