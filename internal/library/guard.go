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

// Owned reports whether path is strictly inside one of the three trees the app may touch:
// Navidrome/, _explaylist_data/Navidrome - *.m3u and .subsonic/ under root.
func Owned(root, path string) bool {
	rel, err := filepath.Rel(resolve(root+"/."), resolve(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	switch parts[0] {
	case "Navidrome", ".subsonic":
		return len(parts) > 1
	case "_explaylist_data":
		return len(parts) == 2 && strings.HasPrefix(parts[1], "Navidrome - ") && strings.HasSuffix(parts[1], ".m3u")
	}
	return false
}

// Remove is the only way the app deletes: it refuses anything not Owned, treats a missing file
// as done, and prunes folders left empty under Navidrome/ (never Navidrome/ itself).
func Remove(root, path string) error {
	if !Owned(root, path) {
		return errors.New("refusing to remove outside owned trees: " + path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	top := filepath.Join(resolve(root+"/."), "Navidrome")
	for d := filepath.Dir(resolve(path)); strings.HasPrefix(d, top+string(filepath.Separator)); d = filepath.Dir(d) {
		if os.Remove(d) != nil { // fails while non-empty
			break
		}
	}
	return nil
}
