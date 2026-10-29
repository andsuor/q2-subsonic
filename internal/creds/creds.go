// Package creds keeps the Navidrome sign-in in the add-on's folder on the card.
package creds

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/andsuor/q2-subsonic/internal/q2"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

const name = "credentials"

// Load reads the saved sign-in; false when there is none or it is incomplete.
func Load(dir string) (subsonic.Creds, bool) {
	var c subsonic.Creds
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || json.Unmarshal(b, &c) != nil || c.Server == "" || c.User == "" {
		return subsonic.Creds{}, false
	}
	return c, true
}

// Save writes the sign-in with mode 0600. The temp file is made 0600 first, as WriteFile keeps the
// mode of a file it truncates, so the password is never readable by others.
func Save(dir string, c subsonic.Creds) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p+".tmp", nil, 0o600); err != nil {
		return err
	}
	if err := q2.WriteFile(p, b); err != nil {
		return err
	}
	return os.Chmod(p, 0o600)
}

// Remove signs out; a missing file is fine.
func Remove(dir string) error {
	if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
