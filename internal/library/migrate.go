package library

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Migrate is the one-time move of the old <root>/Navidrome/ tree to <root>/Subsonic/. It runs when
// the manifest has a path under Navidrome/, or Navidrome/ exists and Subsonic/ does not.
//
// Fast path (no Subsonic/): one rename of the folder, so finished files, stray .part files and
// covers all move along. Slow path (Subsonic/ exists): each manifest entry's file, and its .part,
// moves on its own; whatever the manifest doesn't know stays in Navidrome/. Either way manifest
// paths are rewritten, the recorded playlists are marked for rewrite (their paths changed) and an
// older build's pending.json goes, since the next sync plans again. On error nothing is lost: the fast path
// renames back, the slow path saves the entries it did move, and the error says what was left.
func Migrate(root string, logf func(string, ...any)) error {
	manPath := filepath.Join(root, ".subsonic", "manifest.json")
	m, err := LoadManifest(manPath)
	if err != nil {
		logf("migrate: manifest unreadable, skipping: %v", err)
		return nil
	}
	old, dst := filepath.Join(root, "Navidrome"), filepath.Join(root, "Subsonic")
	under := func(p string) bool { return strings.HasPrefix(p, old+string(filepath.Separator)) }
	legacy := false
	for _, e := range m.Songs {
		legacy = legacy || under(e.Path)
	}
	_, oldErr := os.Stat(old)
	_, dstErr := os.Stat(dst)
	haveOld, haveDst := oldErr == nil, dstErr == nil
	if !legacy && !(haveOld && !haveDst) {
		return nil
	}
	rewrite := func(p string) string { return dst + strings.TrimPrefix(p, old) }
	done := func() error {
		m.PlaylistsStale = true // also covers unrecorded "Navidrome - *.m3u" from older builds: the sync rewrites and removes them
		if err := m.Save(root, manPath); err != nil {
			return err
		}
		return Remove(root, filepath.Join(root, ".subsonic", "pending.json"))
	}

	if haveOld && !haveDst {
		if err := os.Rename(old, dst); err != nil {
			logf("migrate: Navidrome/ -> Subsonic/: %v", err)
			return err
		}
		saved := map[string]string{}
		for id, e := range m.Songs {
			if under(e.Path) {
				saved[id], e.Path = e.Path, rewrite(e.Path)
			}
		}
		if err := done(); err != nil {
			for id, p := range saved {
				m.Songs[id].Path = p
			}
			if rerr := os.Rename(dst, old); rerr != nil {
				logf("migrate: could not move Subsonic/ back: %v", rerr)
			}
			logf("migrate: %v; left Navidrome/ as it was", err)
			return err
		}
		logf("migrate: moved Navidrome/ to Subsonic/ (%d songs)", len(saved))
		return nil
	}

	moved, failed := 0, 0
	var first error // the first song that couldn't move, which the error names
	for id, e := range m.Songs {
		if !under(e.Path) {
			continue
		}
		to := rewrite(e.Path)
		err := moveOwned(root, e.Path, to)
		if errors.Is(err, os.ErrNotExist) { // already moved (a crash after the rename), or gone: follow the new layout
			err = nil
		}
		if err != nil {
			logf("migrate: %s: %v", e.Path, err)
			failed++
			if first == nil {
				first = err
			}
			continue
		}
		if _, err := os.Stat(e.Path + ".part"); err == nil {
			if err := moveOwned(root, e.Path+".part", to+".part"); err != nil {
				logf("migrate: %s.part: %v", e.Path, err)
			}
		}
		m.Songs[id].Path = to
		moved++
	}
	if moved > 0 || failed == 0 {
		if err := done(); err != nil {
			return err
		}
	}
	pruneEmpty(old)
	logf("migrate: moved %d songs from Navidrome/ into the existing Subsonic/, %d left", moved, failed)
	if failed > 0 {
		return fmt.Errorf("%d songs left in Navidrome/: %w", failed, first)
	}
	return nil
}

// moveOwned renames from (which must be inside the legacy Navidrome/ tree, symlinks resolved) to
// to (which must be Owned, so inside Subsonic/), never over a file: one there already is a
// *PathError for to with os.ErrExist. A missing from is os.ErrNotExist, so the caller can tell a
// move that already happened.
func moveOwned(root, from, to string) error {
	top := resolve(filepath.Join(root, "Navidrome"))
	if rf := resolve(from); filepath.Clean(from) != from || !strings.HasPrefix(rf, top+string(filepath.Separator)) {
		return errors.New("refusing path outside the legacy tree: " + from)
	}
	if err := checkOwned(root, to, nil); err != nil {
		return err
	}
	if _, err := os.Lstat(from); err != nil {
		return err
	}
	if _, err := os.Lstat(to); err == nil {
		return &os.PathError{Op: "move", Path: to, Err: os.ErrExist}
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.Rename(from, to)
}

// pruneEmpty removes the empty folders the migration left, bottom up, Navidrome/ itself last.
// os.Remove refuses a folder with anything in it.
func pruneEmpty(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() {
			pruneEmpty(filepath.Join(dir, e.Name()))
		}
	}
	os.Remove(dir)
}
