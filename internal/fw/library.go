package fw

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/andsuor/q2-subsonic/internal/q2"
)

// RequestLibrary writes a library request to path (atomically via q2.WriteFile) (R35):
// <seq>, then "scan" when asked, then "import <path>" for each playlist the app owns (a playlist
// of that name on the Q2 is the app's earlier import, and is replaced) and "add <path>" for each
// it doesn't yet (skipped when the Q2 has a playlist of that name: the user's own).
// Each path must be directly in a card's _explaylist_data/ (no nesting, no ".."), end in .m3u,
// and contain no newline or carriage return (line injection prevention).
// On any validation error, nothing is written.
func RequestLibrary(path string, seq int64, scan bool, imports, adds []string) error {
	for _, m3u := range append(append([]string(nil), imports...), adds...) {
		if err := checkM3U(m3u); err != nil {
			return err
		}
	}
	var sb strings.Builder
	sb.WriteString(strconv.FormatInt(seq, 10) + "\n")
	if scan {
		sb.WriteString("scan\n")
	}
	for _, m3u := range imports {
		sb.WriteString("import " + m3u + "\n")
	}
	for _, m3u := range adds {
		sb.WriteString("add " + m3u + "\n")
	}
	return q2.WriteFile(path, []byte(sb.String()))
}

func checkM3U(m3u string) error {
	// Reject paths containing newline or carriage return
	if strings.ContainsAny(m3u, "\n\r") {
		return fmt.Errorf("import path contains newline/carriage return: %q", m3u)
	}
	// Path must be clean (no ".." or redundant separators)
	if filepath.Clean(m3u) != m3u {
		return fmt.Errorf("import path is not clean: %s", m3u)
	}
	// Directory must be a card's _explaylist_data (the firmware checks it is /mnt/mmc's)
	if !filepath.IsAbs(m3u) || filepath.Base(filepath.Dir(m3u)) != "_explaylist_data" {
		return fmt.Errorf("import path not directly in _explaylist_data/: %s", m3u)
	}
	// Base name must end in .m3u, with a name before the extension
	base := filepath.Base(m3u)
	if !strings.HasSuffix(base, ".m3u") || base == ".m3u" {
		return fmt.Errorf("import path is not a named .m3u: %s", m3u)
	}
	// The firmware refuses ".." anywhere in the name, and with it the whole request
	if strings.Contains(base, "..") {
		return fmt.Errorf("import path has \"..\" in its name: %s", m3u)
	}
	return nil
}

// ReadRequest reads back the request at path: its seq (0 when there is none) and the playlist
// files it imports and adds.
func ReadRequest(path string) (seq int64, imports, adds []string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, nil, nil
	}
	lines := strings.Split(string(data), "\n")
	if seq, err = strconv.ParseInt(lines[0], 10, 64); err != nil {
		return 0, nil, nil
	}
	for _, l := range lines[1:] {
		if p, ok := strings.CutPrefix(l, "import "); ok {
			imports = append(imports, p)
		} else if p, ok := strings.CutPrefix(l, "add "); ok {
			adds = append(adds, p)
		}
	}
	return seq, imports, adds
}

// LibraryDone reads the library done file at path.
// Format: "<seq> ok" or "<seq> error <message>", then "exists <path>" lines (R35).
// Returns (done, ok, msg, exists):
// - done: whether the seq matches (meaning this request is complete)
// - ok: whether the operation succeeded (true for "ok", false for "error")
// - msg: the error message (empty for "ok")
// - exists: the "add" paths skipped because the Q2 has a playlist of that name
func LibraryDone(path string, seq int64) (done, ok bool, msg string, exists []string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, false, "", nil
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	parts := strings.SplitN(strings.TrimSpace(lines[0]), " ", 3)

	// Parse the sequence number; another seq means this request is not done yet
	fileSeq, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || fileSeq != seq {
		return false, false, "", nil
	}
	for _, l := range lines[1:] {
		if p, found := strings.CutPrefix(strings.TrimSpace(l), "exists "); found {
			exists = append(exists, p)
		}
	}
	switch {
	case len(parts) < 2:
		return true, false, "", exists
	case parts[1] == "ok":
		return true, true, "", exists
	case parts[1] == "error" && len(parts) > 2:
		return true, false, parts[2], exists // the rest of the line, verbatim
	}
	return true, false, "", exists
}
