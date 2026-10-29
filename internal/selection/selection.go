// Package selection models .subsonic/selection.json, which a future web
// client also edits, so unknown top-level fields are preserved.
package selection

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/andsuor/q2-subsonic/internal/q2"
)

type Kind string

const (
	KindPlaylist Kind = "playlist"
	KindAlbum    Kind = "album"
	KindArtist   Kind = "artist"
	KindSong     Kind = "song"
)

type Item struct {
	Kind    Kind      `json:"kind"`
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Quality string    `json:"quality"`
	Added   time.Time `json:"added"`
}

// Set is the selection. Exclude is reserved: never read, always written [].
type Set struct {
	Version int
	Items   []Item
	Exclude []string
	extra   map[string]json.RawMessage // unknown top-level keys
}

func Key(kind Kind, id string) string { return string(kind) + ":" + id }

// Load returns an empty Set (version 1) when path does not exist.
func Load(path string) (Set, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Set{Version: 1}, nil
	}
	if err != nil {
		return Set{}, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return Set{}, err
	}
	s := Set{Version: 1}
	if v, ok := m["version"]; ok {
		if err := json.Unmarshal(v, &s.Version); err != nil {
			return Set{}, err
		}
	}
	if v, ok := m["items"]; ok {
		if err := json.Unmarshal(v, &s.Items); err != nil {
			return Set{}, err
		}
	}
	delete(m, "version")
	delete(m, "items")
	delete(m, "exclude")
	s.extra = m
	return s, nil
}

func (s Set) Save(path string) error {
	m := map[string]any{}
	for k, v := range s.extra {
		m[k] = v
	}
	items := s.Items
	if items == nil {
		items = []Item{}
	}
	m["version"], m["items"], m["exclude"] = s.Version, items, []string{}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return q2.WriteFile(path, b)
}

func (s Set) index(kind Kind, id string) int {
	for i, it := range s.Items {
		if it.Kind == kind && it.ID == id {
			return i
		}
	}
	return -1
}

func (s Set) Has(kind Kind, id string) bool { return s.index(kind, id) >= 0 }

// Toggle removes a present item (false) or appends it (true), stamping Added if zero.
func (s *Set) Toggle(it Item) bool {
	if i := s.index(it.Kind, it.ID); i >= 0 {
		s.Items = append(s.Items[:i], s.Items[i+1:]...)
		return false
	}
	if it.Added.IsZero() {
		it.Added = time.Now().UTC()
	}
	s.Items = append(s.Items, it)
	return true
}

func (s *Set) SetQuality(kind Kind, id, q string) {
	if i := s.index(kind, id); i >= 0 {
		s.Items[i].Quality = q
	}
}
