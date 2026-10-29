package applog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	got := Redact("https://x/rest/ping?u=a&t=abc&s=def&p=enc:1")
	for _, bad := range []string{"abc", "def", "enc:1"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%q leaked in %q", bad, got)
		}
	}
	if !strings.Contains(got, "u=a") {
		t.Fatalf("lost u=a: %q", got)
	}
}

func TestRotate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log.txt")
	l := Open(p, 100)
	for i := 0; i < 20; i++ {
		l.Printf("line number %d", i)
	}
	st, err := os.Stat(p)
	if err != nil || st.Size() > 100+60 {
		t.Fatalf("log.txt size %v err %v", st, err)
	}
	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatal("no rotated file:", err)
	}
}
