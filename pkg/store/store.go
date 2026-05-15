package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type StoreFile string

const (
	filePerm = 0644

	// data files.
	Themes        StoreFile = "themes.json"
	Plugins       StoreFile = "plugins.json"
	Resolved      StoreFile = "resolved.json"
	Conflicts     StoreFile = "conflicts.json"
	ClosedThemes  StoreFile = "closed-themes.json"
	ClosedPlugins StoreFile = "closed-plugins.json"

	// svn state files.
	ThemeSvnRepoRev  StoreFile = ".theme_last_rev"
	PluginSvnRepoRev StoreFile = ".plugin_last_rev"
)

func (sf StoreFile) Path() string {
	return string(sf)
}

func (sf StoreFile) IsDataFile() bool {
	switch sf {
	case Themes, Plugins, Resolved, Conflicts, ClosedThemes, ClosedPlugins:
		return true
	default:
		return false
	}
}

func (sf StoreFile) IsSvnStateFile() bool {
	switch sf {
	case ThemeSvnRepoRev, PluginSvnRepoRev:
		return true
	default:
		return false
	}
}

type PackageType string

const (
	Theme  PackageType = "theme"
	Plugin PackageType = "plugin"
)

func (pt PackageType) Valid() bool {
	return pt == Theme || pt == Plugin
}

// revFile returns the svn-revision state file for this package type.
// Returns an error if the package type is invalid.
func (pt PackageType) revFile() (StoreFile, error) {
	switch pt {
	case Theme:
		return ThemeSvnRepoRev, nil
	case Plugin:
		return PluginSvnRepoRev, nil
	default:
		return "", fmt.Errorf("invalid package type: %s", pt)
	}
}

type PackageClosure string

const (
	ClosureUnknown   PackageClosure = "unknown"
	ClosureTemporary PackageClosure = "temporary"
	ClosurePermanent PackageClosure = "permanent"
)

func (pc PackageClosure) String() string {
	return string(pc)
}

func (pc PackageClosure) Valid() bool {
	switch pc {
	case ClosureUnknown, ClosureTemporary, ClosurePermanent:
		return true
	default:
		return false
	}
}

type ResolvedConfig struct {
	Themes  []string `json:"themes"`
	Plugins []string `json:"plugins"`
}

// atomicWrite writes to path durably: tmp file in the same directory,
// fsync, then rename. From any outside observer the file contains either
// the old contents or the new contents — never anything in between.
func atomicWrite(path string, write func(io.Writer) error) (err error) {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			os.Remove(tmpName)
		}
	}()

	closed := false
	closeOnce := func() error {
		if closed {
			return nil
		}
		closed = true
		return tmp.Close()
	}
	defer closeOnce()

	if err := write(tmp); err != nil {
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(filePerm); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", tmpName, err)
	}
	if err := closeOnce(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}

// GetLastSvnRevision returns the last persisted svn revision for pkgType.
// A missing or empty state file is treated as revision 0.
func GetLastSvnRevision(pkgType PackageType) (int, error) {
	revFile, err := pkgType.revFile()
	if err != nil {
		return 0, err
	}

	data, err := os.ReadFile(revFile.Path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read %s: %w", revFile, err)
	}

	s := strings.TrimSpace(string(data))
	if s == "" {
		return 0, nil
	}

	rev, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parse revision from %s: %w", revFile, err)
	}
	return rev, nil
}

func SetLastSvnRevision(pkgType PackageType, rev int) error {
	revFile, err := pkgType.revFile()
	if err != nil {
		return err
	}

	return atomicWrite(revFile.Path(), func(w io.Writer) error {
		_, err := io.WriteString(w, strconv.Itoa(rev))
		return err
	})
}

// getData reads the specified data file and unmarshals it into dest.
// If the file does not exist, dest is left unchanged and no error is returned.
func getData(file StoreFile, dest any) error {
	if !file.IsDataFile() {
		return fmt.Errorf("file %s is not a data file", file)
	}

	path := file.Path()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil
	}

	if err := json.Unmarshal(data, dest); err != nil {
		return fmt.Errorf("unmarshal %s: %w", path, err)
	}
	return nil
}

// setData marshals data and writes it to the specified file atomically.
func setData(file StoreFile, data any) error {
	if !file.IsDataFile() {
		return fmt.Errorf("file %s is not a data file", file)
	}

	return atomicWrite(file.Path(), func(w io.Writer) error {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(data)
	})
}

// GetPlugins returns the data from plugins.json
func GetPlugins() ([]string, error) {
	var plugins []string
	if err := getData(Plugins, &plugins); err != nil {
		return nil, fmt.Errorf("get plugins: %w", err)
	}
	return plugins, nil
}

// SetPlugins writes the given plugins list to plugins.json
func SetPlugins(plugins []string) error {
	return setData(Plugins, plugins)
}

// GetThemes returns the data from themes.json
func GetThemes() ([]string, error) {
	var themes []string
	if err := getData(Themes, &themes); err != nil {
		return nil, fmt.Errorf("get themes: %w", err)
	}
	return themes, nil
}

// SetThemes writes the given themes list to themes.json
func SetThemes(themes []string) error {
	return setData(Themes, themes)
}

// GetResolved returns the data from resolved.json
func GetResolved() (ResolvedConfig, error) {
	var resolved ResolvedConfig
	if err := getData(Resolved, &resolved); err != nil {
		return ResolvedConfig{}, fmt.Errorf("get resolved config: %w", err)
	}
	return resolved, nil
}

// SetResolved writes the given resolved config to resolved.json
func SetResolved(resolved ResolvedConfig) error {
	return setData(Resolved, resolved)
}

// GetConflicts returns the data from conflicts.json
func GetConflicts() ([]string, error) {
	var conflicts []string
	if err := getData(Conflicts, &conflicts); err != nil {
		return nil, fmt.Errorf("get conflicts: %w", err)
	}
	return conflicts, nil
}

// SetConflicts writes the given conflicts list to conflicts.json
func SetConflicts(conflicts []string) error {
	return setData(Conflicts, conflicts)
}

// GetClosedThemes returns the data from closed-themes.json
func GetClosedThemes() (map[string]PackageClosure, error) {
	var closedThemes map[string]PackageClosure
	err := getData(ClosedThemes, &closedThemes)
	if closedThemes == nil {
		closedThemes = make(map[string]PackageClosure)
	}
	if err != nil {
		return closedThemes, fmt.Errorf("get closed themes: %w", err)
	}
	return closedThemes, nil
}

// SetClosedThemes writes the given closed themes map to closed-themes.json
func SetClosedThemes(closedThemes map[string]PackageClosure) error {
	return setData(ClosedThemes, closedThemes)
}

// GetClosedPlugins returns the data from closed-plugins.json
func GetClosedPlugins() (map[string]PackageClosure, error) {
	var closedPlugins map[string]PackageClosure
	err := getData(ClosedPlugins, &closedPlugins)
	if closedPlugins == nil {
		closedPlugins = make(map[string]PackageClosure)
	}
	if err != nil {
		return closedPlugins, fmt.Errorf("get closed plugins: %w", err)
	}
	return closedPlugins, nil
}

// SetClosedPlugins writes the given closed plugins map to closed-plugins.json
func SetClosedPlugins(closedPlugins map[string]PackageClosure) error {
	return setData(ClosedPlugins, closedPlugins)
}

// GetPackages returns the list of packages of the given type (themes or plugins).
func GetPackages(pkgType PackageType) ([]string, error) {
	switch pkgType {
	case Theme:
		return GetThemes()
	case Plugin:
		return GetPlugins()
	default:
		return nil, fmt.Errorf("invalid package type: %s", pkgType)
	}
}

// GetClosedPackages returns the map of closed packages of the given type (themes or plugins).
func GetClosedPackages(pkgType PackageType) (map[string]PackageClosure, error) {
	switch pkgType {
	case Theme:
		return GetClosedThemes()
	case Plugin:
		return GetClosedPlugins()
	default:
		return nil, fmt.Errorf("invalid package type: %s", pkgType)
	}
}
