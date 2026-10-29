// Package applog is a small append-per-line log that never keeps its file open, so the card is
// never busy, and that rotates to <path>.1 at a size limit.
package applog

import (
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"
)

type Log struct {
	path string
	max  int64
	mu   sync.Mutex
}

// Open returns a log at path that rotates once the file would pass max bytes.
func Open(path string, max int64) *Log { return &Log{path: path, max: max} }

// Printf appends one timestamped line, opening and closing the file each time.
func (l *Log) Printf(format string, args ...any) {
	line := time.Now().UTC().Format("2006-01-02 15:04:05 ") + Redact(fmt.Sprintf(format, args...)) + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	if st, err := os.Stat(l.path); err == nil && st.Size()+int64(len(line)) > l.max {
		os.Rename(l.path, l.path+".1")
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	f.WriteString(line)
	f.Close()
}

var (
	secret   = regexp.MustCompile(`(?i)([?&](?:p|t|s|password)=)[^&\s"]*`)
	userinfo = regexp.MustCompile(`(://[^/@\s":]*:)[^/\s"]*@`)
)

// Redact replaces the values of the query parameters p, t, s and password, and the password in a
// URL's user:password@, with ***.
func Redact(s string) string {
	return userinfo.ReplaceAllString(secret.ReplaceAllString(s, "${1}***"), "${1}***@")
}
