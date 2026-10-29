package library

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/andsuor/q2-subsonic/internal/q2"
)

// Entry is one synced song. Meta is MetaKey at sync time; Items are the selection keys holding it.
// Size is the bytes on the card; SrcSize, for an original, the server's size of the file it came
// from (0 when unknown: synced before it was kept), so a re-rip is noticed.
type Entry struct {
	ID, Path, Quality string
	Size              int64
	SrcSize           int64 `json:",omitempty"`
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
	// Imported holds the playlist files the firmware confirmed importing (R35): the Q2 playlist of
	// that name is the app's, so later requests replace it ("import"); any other file is sent as
	// "add", which never touches a playlist the user made. A file stays here after it is removed,
	// as its playlist stays on the Q2.
	Imported map[string]bool
	// PlaylistSums holds, per playlist name, the PlaylistSum of the song paths last written for it:
	// a playlist whose songs or order changed is written again though no song moves (I10).
	PlaylistSums map[string]string `json:",omitempty"`
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
	if m.PlaylistSums == nil {
		m.PlaylistSums = map[string]string{}
	}
	if m.Imported == nil { // an older manifest: its playlists were imported by name-replacing requests
		m.Imported = map[string]bool{}
		for _, f := range m.Playlists {
			m.Imported[f] = true
		}
	}
	return m, err
}

// MovePlaylist renames the .m3u recorded for playlist name to to, a free file directly in
// <root>/_explaylist_data/, and records it there.
func (m *Manifest) MovePlaylist(root, name, to string) error {
	from, ok := m.Playlists[name]
	if !ok {
		return errors.New("no playlist file recorded for " + name)
	}
	if err := checkOwned(root, from, m); err != nil {
		return err
	}
	if !ValidPlaylistFile(root, to) {
		return errors.New("refusing playlist path: " + to)
	}
	if _, err := os.Lstat(to); err == nil {
		return errors.New("already exists: " + to)
	}
	if err := os.Rename(from, to); err != nil {
		return err
	}
	m.Playlists[name] = to
	return nil
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
