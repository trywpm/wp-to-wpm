package svn

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"wpm-migration/pkg/store"
	"wpm-migration/pkg/unsafeconv"
	"wpm-migration/pkg/validate"
)

const (
	themesSvnRepo  = "https://themes.svn.wordpress.org"
	pluginsSvnRepo = "https://plugins.svn.wordpress.org"
)

func List(ctx context.Context, pkgType store.PackageType) (map[string]struct{}, error) {
	var svnRepo string
	if pkgType == store.Theme {
		svnRepo = themesSvnRepo
	} else {
		svnRepo = pluginsSvnRepo
	}

	entries, err := listEntries(ctx, svnRepo, validate.PackageName)
	if err != nil {
		return nil, err
	}

	names := make(map[string]struct{}, len(entries))
	for name := range entries {
		names[name] = struct{}{}
	}

	return names, nil
}

// ListPluginTags lists all tags for a given plugin slug, keyed by the original
// SVN tag name and valued by the tag's last commit date.
func ListPluginTags(ctx context.Context, pluginSlug string) (map[string]time.Time, error) {
	if !validate.PackageName(unsafeconv.StringToBytes(pluginSlug)) {
		return nil, fmt.Errorf("invalid plugin slug: %s", pluginSlug)
	}

	// svn plugin repo stores tags under /tags/ subdirectory.
	svnRepo := pluginsSvnRepo + "/" + pluginSlug + "/tags/"

	return listEntries(ctx, svnRepo, nil)
}

// ListThemeTags lists all tags for a given theme slug, keyed by the original
// SVN tag name and valued by the tag's last commit date.
func ListThemeTags(ctx context.Context, themeSlug string) (map[string]time.Time, error) {
	if !validate.PackageName(unsafeconv.StringToBytes(themeSlug)) {
		return nil, fmt.Errorf("invalid theme slug: %s", themeSlug)
	}

	svnRepo := themesSvnRepo + "/" + themeSlug

	return listEntries(ctx, svnRepo, nil)
}

func ListTags(ctx context.Context, pkgType store.PackageType, slug string) (map[string]time.Time, error) {
	switch pkgType {
	case store.Theme:
		return ListThemeTags(ctx, slug)
	case store.Plugin:
		return ListPluginTags(ctx, slug)
	default:
		return nil, fmt.Errorf("invalid package type: %s", pkgType)
	}
}

type svnListEntry struct {
	Name   string `xml:"name"`
	Kind   string `xml:"kind,attr"`
	Commit struct {
		Date string `xml:"date"`
	} `xml:"commit"`
}

type svnListResult struct {
	Entries []svnListEntry `xml:"list>entry"`
}

// listEntries lists directory entries, keyed by name and valued by the entry's
// last commit date. An unparseable or missing date yields the zero time.
func listEntries(ctx context.Context, svnRepo string, isValid func([]byte) bool) (map[string]time.Time, error) {
	cmd := exec.CommandContext(ctx, "svn", "list", "--xml", "--non-interactive", svnRepo)
	cmd.Env = append(os.Environ(), "LC_ALL=", "LC_MESSAGES=C")

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		errMsg := stderrBuf.String()
		if strings.Contains(errMsg, "non-existent") {
			return map[string]time.Time{}, nil
		}

		return nil, fmt.Errorf("svn list failed for %s: %w\nstderr: %s", svnRepo, err, strings.TrimSpace(errMsg))
	}

	var result svnListResult
	if err := xml.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("failed to parse svn list xml from %s: %w", svnRepo, err)
	}

	listMap := make(map[string]time.Time, len(result.Entries))
	for _, entry := range result.Entries {
		if entry.Kind != "dir" {
			continue
		}

		if entry.Name == "" || entry.Name == "." || entry.Name == ".." {
			continue
		}

		if isValid != nil && !isValid(unsafeconv.StringToBytes(entry.Name)) {
			continue
		}

		var date time.Time
		if entry.Commit.Date != "" {
			if d, err := time.Parse(time.RFC3339Nano, entry.Commit.Date); err == nil {
				date = d
			}
		}

		listMap[entry.Name] = date
	}

	return listMap, nil
}

type SvnLogEntry struct {
	Revision string    `xml:"revision,attr"`
	Paths    []SvnPath `xml:"paths>path"`
}

type SvnPath struct {
	Path string `xml:",chardata"`
}

type svnLogResult struct {
	Entries []SvnLogEntry `xml:"logentry"`
}

// GetUpdatedPackages returns the packages changed in the revision range
// (startRev, cutoff], where cutoff is resolved to a concrete revision via svn's
// {DATE} specifier. Revisions newer than cutoff (still within the cooldown) are
// never fetched; the returned head revision is the resolved cutoff boundary, so
// successive runs tile the revision space exactly: continuing from one run's
// boundary to the next never skips or re-processes a revision.
func GetUpdatedPackages(ctx context.Context, pkgType store.PackageType, startRev int, cutoff time.Time) ([]string, int, error) {
	if startRev <= 0 {
		return nil, 0, fmt.Errorf("invalid start revision: %d", startRev)
	}

	var svnRepoURL string
	switch pkgType {
	case store.Plugin:
		svnRepoURL = pluginsSvnRepo
	case store.Theme:
		svnRepoURL = themesSvnRepo
	default:
		return nil, 0, fmt.Errorf("invalid package type: %s", pkgType)
	}

	// Resolve the cooldown cutoff to the youngest revision at or before it.
	// Everything up to here has aged past the cooldown; newer revisions wait.
	cutoffRev, err := revAtDate(ctx, svnRepoURL, cutoff)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, fmt.Errorf("failed to resolve cutoff revision: %w", err)
	}

	// Nothing has aged past the cutoff since the last run; hold the pointer.
	if cutoffRev < startRev {
		return []string{}, startRev - 1, nil
	}

	revisionRange := fmt.Sprintf("%d:%d", startRev, cutoffRev)

	cmd := exec.CommandContext(ctx, "svn", "log", "--xml", "-q", "-v", "--non-interactive", "-r", revisionRange, svnRepoURL)
	cmd.Env = append(os.Environ(), "LC_ALL=", "LC_MESSAGES=C")

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}

		errMsg := stderrBuf.String()
		if strings.Contains(errMsg, "E160006") {
			return []string{}, startRev - 1, nil
		}

		return nil, 0, fmt.Errorf("svn log failed: %w\nstderr: %s", err, strings.TrimSpace(errMsg))
	}

	var result svnLogResult
	if err := xml.Unmarshal(out, &result); err != nil {
		return nil, 0, fmt.Errorf("failed to parse svn log xml: %w", err)
	}

	packageSet := make(map[string]struct{})
	for _, entry := range result.Entries {
		for _, p := range entry.Paths {
			parts := strings.Split(strings.Trim(p.Path, "/"), "/")
			if len(parts) <= 1 {
				continue
			}

			slug := parts[0]
			if slug == "" || slug == "." || slug == ".." {
				continue
			}

			packageSet[slug] = struct{}{}
		}
	}

	updatedPackages := make([]string, 0, len(packageSet))
	for pkg := range packageSet {
		if !validate.PackageName(unsafeconv.StringToBytes(pkg)) {
			continue
		}

		updatedPackages = append(updatedPackages, pkg)
	}

	sort.Strings(updatedPackages)

	// Advance to the resolved cutoff, not merely the highest revision that
	// touched a package, so the pointer never lags behind an aged revision.
	return updatedPackages, cutoffRev, nil
}

// revAtDate resolves a timestamp to the youngest repository revision committed
// at or before it, using svn's {DATE} revision specifier.
func revAtDate(ctx context.Context, svnRepoURL string, date time.Time) (int, error) {
	spec := "{" + date.UTC().Format("2006-01-02T15:04:05Z") + "}"

	cmd := exec.CommandContext(ctx, "svn", "info", "--show-item", "revision", "--non-interactive", "-r", spec, svnRepoURL)
	cmd.Env = append(os.Environ(), "LC_ALL=", "LC_MESSAGES=C")

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("svn info failed: %w\nstderr: %s", err, strings.TrimSpace(stderrBuf.String()))
	}

	rev, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("failed to parse revision %q: %w", strings.TrimSpace(string(out)), err)
	}

	return rev, nil
}

func UpdatedPlugins(ctx context.Context, startRev int, cutoff time.Time) ([]string, int, error) {
	return GetUpdatedPackages(ctx, store.Plugin, startRev, cutoff)
}

func UpdatedThemes(ctx context.Context, startRev int, cutoff time.Time) ([]string, int, error) {
	return GetUpdatedPackages(ctx, store.Theme, startRev, cutoff)
}

// Export checks out a single tag of a package from SVN into a temp directory.
// The returned cleanup function must be called once the caller is done with
// the export, even if Export returned an error from a later call.
func Export(ctx context.Context, pkgType store.PackageType, name, tag string) (path string, cleanup func(), err error) {
	var svnTagUrl string
	switch pkgType {
	case store.Plugin:
		svnTagUrl = pluginsSvnRepo + "/" + name + "/tags/" + tag
	case store.Theme:
		svnTagUrl = themesSvnRepo + "/" + name + "/" + tag
	default:
		return "", nil, fmt.Errorf("invalid package type: %s", pkgType)
	}

	tempDir, err := os.MkdirTemp("", "svn-checkout-*")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temporary directory: %w", err)
	}

	checkoutPath := filepath.Join(tempDir, name, tag)

	cmd := exec.CommandContext(ctx, "svn", "export", "-q", "--non-interactive", svnTagUrl, checkoutPath)
	cmd.Env = append(os.Environ(), "LC_ALL=", "LC_MESSAGES=C") // Ensure error messages are in English for consistent error handling
	if o, err := cmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(tempDir)
		return "", nil, fmt.Errorf("svn export failed: %w\noutput: %s", err, string(o))
	}

	return checkoutPath, func() {
		_ = os.RemoveAll(tempDir)
	}, nil
}
