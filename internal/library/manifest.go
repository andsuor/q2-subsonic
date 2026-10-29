package library

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/andsuor/q2-subsonic/internal/q2"
)

// Entry is one synced song. Meta is MetaKey at sync time; Items are the selection keys holding it.
type Entry struct {
	ID, Path, Quality string
	Size              int64
	Items             []string
	Meta              string
}

// Manifest is what the app has put on the card. Playlists maps a playlist name to the absolute
// path of the .m3u written for it, the only playlist files the app may overwrite or delete.
type Manifest struct {
	Songs     map[string]*Entry
	Playlists map[string]string
	// PlaylistsStale: the recorded .m3u files hold old paths (after a folder migration), so the next
	// sync rewrites every one even when no song changes. The syncer clears it.
	PlaylistsStale bool `json:",omitempty"`
}

// LoadManifest reads path; a missing file is an empty manifest.
func LoadManifest(path string) (*Manifest, error) {
	m := &Manifest{}
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, m)
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if m.Songs == nil {
		m.Songs = map[string]*Entry{}
	}
	if m.Playlists == nil {
		m.Playlists = map[string]string{}
	}
	return m, err
}

// Save writes the manifest atomically.
func (m *Manifest) Save(root, path string) error {
	if err := checkOwned(root, path, m); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return q2.WriteFile(path, b)
}
