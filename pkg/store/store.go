package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// files storing state data.
	// must be stored in the project's root.
	ThemesJson        = "themes.json"
	PluginsJson       = "plugins.json"
	ResolvedJson      = "resolved.json"
	ConflictsJson     = "conflicts.json"
	ClosedThemesJson  = "closed-themes.json"
	ClosedPluginsJson = "closed-plugins.json"

	// svn state files.
	ThemeSvnRepoRev  = ".theme_last_rev"
	PluginSvnRepoRev = ".plugin_last_rev"
)

type PackageType string

const (
	theme  PackageType = "theme"
	plugin PackageType = "plugin"
)

func (pt PackageType) Valid() bool {
	return pt == theme || pt == plugin
}

func GetLastSvnRevision(pkgType PackageType) (int, error) {
	if !pkgType.Valid() {
		return 0, fmt.Errorf("invalid package type: %s", pkgType)
	}

	var revFile string
	switch pkgType {
	case theme:
		revFile = ThemeSvnRepoRev
	case plugin:
		revFile = PluginSvnRepoRev
	}

	data, err := os.ReadFile(revFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to read last svn revision from %s: %w", revFile, err)
	}

	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func SetLastSvnRevision(pkgType PackageType, rev int) error {
	if !pkgType.Valid() {
		return fmt.Errorf("invalid package type: %s", pkgType)
	}

	var revFile string
	switch pkgType {
	case theme:
		revFile = ThemeSvnRepoRev
	case plugin:
		revFile = PluginSvnRepoRev
	}

	return os.WriteFile(revFile, []byte(strconv.Itoa(rev)), 0644)
}

func GetData(filename string, dest any) error {
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

func SetData(path string, data any) error {
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
