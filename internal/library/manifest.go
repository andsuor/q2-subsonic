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

type Manifest struct{ Songs map[string]*Entry }

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
	return m, err
}

// Save writes the manifest atomically.
func (m *Manifest) Save(root, path string) error {
	if err := checkOwned(root, path); err != nil {
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
