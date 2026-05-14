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
	StoreThemes        StoreFile = "themes.json"
	StorePlugins       StoreFile = "plugins.json"
	StoreResolved      StoreFile = "resolved.json"
	StoreConflicts     StoreFile = "conflicts.json"
	StoreClosedThemes  StoreFile = "closed-themes.json"
	StoreClosedPlugins StoreFile = "closed-plugins.json"

	// svn state files.
	StoreThemeSvnRepoRev  StoreFile = ".theme_last_rev"
	StorePluginSvnRepoRev StoreFile = ".plugin_last_rev"
)

func (sf StoreFile) Path() string {
	return string(sf)
}

func (sf StoreFile) IsDataFile() bool {
	switch sf {
	case StoreThemes, StorePlugins, StoreResolved, StoreConflicts, StoreClosedThemes, StoreClosedPlugins:
		return true
	default:
		return false
	}
}

func (sf StoreFile) IsSvnStateFile() bool {
	switch sf {
	case StoreThemeSvnRepoRev, StorePluginSvnRepoRev:
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
		return StoreThemeSvnRepoRev, nil
	case Plugin:
		return StorePluginSvnRepoRev, nil
	default:
		return "", fmt.Errorf("invalid package type: %s", pt)
	}
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

// GetData reads the specified data file and unmarshals it into dest.
// If the file does not exist, dest is left unchanged and no error is returned.
func GetData(file StoreFile, dest any) error {
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

func SetData(file StoreFile, data any) error {
	if !file.IsDataFile() {
		return fmt.Errorf("file %s is not a data file", file)
	}

	return atomicWrite(file.Path(), func(w io.Writer) error {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(data)
	})
}
