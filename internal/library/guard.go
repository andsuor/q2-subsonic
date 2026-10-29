package library

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// resolve cleans p and resolves symlinks in its directory (deepest existing ancestor), leaving
// the last element alone so a symlink is judged, and removed, as itself.
func resolve(p string) string {
	p = filepath.Clean(p)
	dir, base := filepath.Split(p)
	dir = filepath.Clean(dir)
	var rest []string
	for {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			dir = r
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		rest = append([]string{filepath.Base(dir)}, rest...)
		dir = parent
	}
	return filepath.Join(append([]string{dir}, append(rest, base)...)...)
}

// Owned reports whether path is strictly inside what the app may touch under root: Subsonic/,
// .subsonic/, and in _explaylist_data/ only the .m3u files recorded in m.Playlists (m may be nil)
// plus legacy "Navidrome - *.m3u" files, which exist only to be migrated away.
func Owned(root, path string, m *Manifest) bool {
	parts := relParts(root, path)
	if parts == nil {
		return false
	}
	switch parts[0] {
	case "Subsonic", ".subsonic":
		return len(parts) > 1
	case "_explaylist_data":
		if len(parts) != 2 || !strings.HasSuffix(parts[1], ".m3u") {
			return false
		}
		if strings.HasPrefix(parts[1], "Navidrome - ") {
			return true
		}
		if m != nil {
			for _, f := range m.Playlists {
				if filepath.Clean(f) == filepath.Clean(path) {
					return true
				}
			}
		}
	}
	return false
}

// relParts splits path, symlinks resolved, relative to root; nil when it is root or outside it.
func relParts(root, path string) []string {
	rel, err := filepath.Rel(resolve(root+"/."), resolve(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	return strings.Split(rel, string(filepath.Separator))
}

// InSongs reports whether path, in Clean form, is strictly inside <root>/Subsonic/: the only place
// a sync writes songs. Owned also allows .subsonic/, which holds the app binary and run script.
func InSongs(root, path string) bool {
	parts := relParts(root, path)
	return filepath.Clean(path) == path && len(parts) > 1 && parts[0] == "Subsonic"
}

// ValidPlaylistFile reports whether f is exactly <root>/_explaylist_data/<name>.m3u in Clean form,
// the only shape a recorded playlist path may have.
func ValidPlaylistFile(root, f string) bool {
	b := filepath.Base(f)
	return filepath.Clean(f) == f && filepath.Dir(f) == filepath.Join(root, "_explaylist_data") &&
		strings.HasSuffix(b, ".m3u") && b != ".m3u"
}

// checkOwned is the gate for every write and delete: path must already be in Clean form (so the
// OS and the guard read the same path, e.g. no "link/.." tricks) and Owned.
func checkOwned(root, path string, m *Manifest) error {
	if filepath.Clean(path) != path || !Owned(root, path, m) {
		return errors.New("refusing path outside owned trees: " + path)
	}
	return nil
}

// Remove is the only way the app deletes: it refuses anything not Owned, treats a missing file
// as done, and prunes folders left empty under Subsonic/ (never Subsonic/ itself).
func Remove(root, path string) error { return remove(root, path, nil) }

// Remove is the package Remove that also accepts the playlist files recorded in m.
func (m *Manifest) Remove(root, path string) error { return remove(root, path, m) }

func remove(root, path string, m *Manifest) error {
	if err := checkOwned(root, path, m); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	top := filepath.Join(resolve(root+"/."), "Subsonic")
	for d := filepath.Dir(resolve(path)); strings.HasPrefix(d, top+string(filepath.Separator)); d = filepath.Dir(d) {
		if os.Remove(d) != nil { // fails while non-empty
			break
		}
	}
	return nil
}
