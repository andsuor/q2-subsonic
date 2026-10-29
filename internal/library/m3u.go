package library

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/andsuor/q2-subsonic/internal/q2"
)

// WriteM3U writes #EXTM3U and one path per line, duplicates kept, atomically. The paths are
// written as given (absolute, /mnt/mmc/...). If the device wants relative paths (Task 12), change only this.
func WriteM3U(root, path string, paths []string) error {
	return (*Manifest)(nil).WriteM3U(root, path, paths)
}

// WriteM3U is the package WriteM3U that also accepts the playlist files recorded in m.
func (m *Manifest) WriteM3U(root, path string, paths []string) error {
	if err := checkOwned(root, path, m); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	for _, p := range paths {
		b.WriteString(p + "\n")
	}
	return q2.WriteFile(path, []byte(b.String()))
}
