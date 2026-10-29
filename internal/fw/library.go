package fw

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/andsuor/q2-subsonic/internal/q2"
)

// RequestLibrary writes a library request to path (atomically via q2.WriteFile).
// Format: <seq>, then "scan", then "import <path>" for each m3u.
// Each import path must be directly in a card's _explaylist_data/ (no nesting, no ".."),
// end in .m3u, and contain no newline or carriage return (line injection prevention).
// On any validation error, nothing is written.
func RequestLibrary(path string, seq int64, m3us []string) error {
	// Validate all import paths before writing anything
	for _, m3u := range m3us {
		// Reject paths containing newline or carriage return
		if strings.ContainsAny(m3u, "\n\r") {
			return fmt.Errorf("import path contains newline/carriage return: %q", m3u)
		}

		// Path must be clean (no ".." or redundant separators)
		cleanPath := filepath.Clean(m3u)
		if cleanPath != m3u {
			return fmt.Errorf("import path is not clean: %s", m3u)
		}

		// Directory must be a card's _explaylist_data (the firmware checks it is /mnt/mmc's)
		if !filepath.IsAbs(m3u) || filepath.Base(filepath.Dir(m3u)) != "_explaylist_data" {
			return fmt.Errorf("import path not directly in _explaylist_data/: %s", m3u)
		}

		// Base name must be non-empty, end in .m3u, and have non-empty name part
		base := filepath.Base(m3u)
		if base == "" || !strings.HasSuffix(base, ".m3u") {
			return fmt.Errorf("import path does not have .m3u extension: %s", m3u)
		}
		// Reject bare ".m3u" (name part before extension must be non-empty)
		namePart := strings.TrimSuffix(base, ".m3u")
		if namePart == "" {
			return fmt.Errorf("import path must have a name before .m3u extension: %s", m3u)
		}
		// The firmware refuses ".." anywhere in the name, and with it the whole request
		if strings.Contains(base, "..") {
			return fmt.Errorf("import path has \"..\" in its name: %s", m3u)
		}
	}

	// Build the request content
	var sb strings.Builder
	sb.WriteString(strconv.FormatInt(seq, 10))
	sb.WriteString("\nscan\n")
	for _, m3u := range m3us {
		sb.WriteString("import ")
		sb.WriteString(m3u)
		sb.WriteString("\n")
	}

	// Write atomically
	return q2.WriteFile(path, []byte(sb.String()))
}

// LibraryDone reads the library done file at path.
// Format: "<seq> ok" or "<seq> error <message>"
// Returns (done, ok, msg):
// - done: whether the seq matches (meaning this request is complete)
// - ok: whether the operation succeeded (true for "ok", false for "error")
// - msg: the error message (empty for "ok")
func LibraryDone(path string, seq int64) (done, ok bool, msg string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, false, ""
	}

	line := strings.TrimSpace(string(data))
	parts := strings.SplitN(line, " ", 3)

	if len(parts) == 0 {
		return false, false, ""
	}

	// Parse the sequence number
	fileSeq, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return false, false, ""
	}

	// If the sequence doesn't match, this request is not done yet
	if fileSeq != seq {
		return false, false, ""
	}

	// Sequence matches, now check the status
	if len(parts) < 2 {
		return true, false, ""
	}

	if parts[1] == "ok" {
		return true, true, ""
	}

	if parts[1] == "error" {
		// The rest of the line is the error message (preserve verbatim)
		if len(parts) > 2 {
			msg = parts[2]
		}
		return true, false, msg
	}

	return true, false, ""
}
