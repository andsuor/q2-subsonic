package creds

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

func TestCredsMode0600(t *testing.T) {
	dir := t.TempDir()
	want := subsonic.Creds{Server: "https://nd.example", User: "a", Password: "p w"}
	if err := Save(dir, want); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "credentials"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("stat %v mode %v", err, fi.Mode())
	}
	if got, ok := Load(dir); !ok || got != want {
		t.Fatalf("load %v %v", got, ok)
	}
	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := Load(dir); ok {
		t.Fatal("still loads after Remove")
	}
	if err := Remove(dir); err != nil {
		t.Fatalf("Remove of missing file: %v", err)
	}
}

// I23: an API-key sign-in keeps only the key, never a password field.
func TestCredsAPIKeyOnly(t *testing.T) {
	dir := t.TempDir()
	want := subsonic.Creds{Server: "https://nd.example", User: "a", APIKey: "k"}
	if err := Save(dir, want); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "credentials")); strings.Contains(string(b), "Password") {
		t.Fatalf("file %s", b)
	}
	if got, ok := Load(dir); !ok || got != want {
		t.Fatalf("load %v %v", got, ok)
	}
}
