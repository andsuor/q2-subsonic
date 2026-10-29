package fw

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/andsuor/q2-subsonic/internal/q2"
)

// RequestLibrary writes a library request to path (atomically via q2.WriteFile).
// Format: <seq>, then "scan", then "import <path>" for each m3u.
// Each import path must be directly in /mnt/mmc/_explaylist_data/ and end in .m3u.
func RequestLibrary(path string, seq int64, m3us []string) error {
	// Validate all import paths before writing anything
	for _, m3u := range m3us {
		if !strings.HasPrefix(m3u, "/mnt/mmc/_explaylist_data/") {
			return fmt.Errorf("import path not in /mnt/mmc/_explaylist_data/: %s", m3u)
		}
		if !strings.HasSuffix(m3u, ".m3u") {
			return fmt.Errorf("import path does not end in .m3u: %s", m3u)
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
	parts := strings.Fields(line)

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
		// The rest of the line is the error message
		if len(parts) > 2 {
			msg = strings.Join(parts[2:], " ")
		}
		return true, false, msg
	}

	return true, false, ""
}
