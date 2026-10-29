package selection

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestToggleAddRemove(t *testing.T) {
	var s Set
	it := Item{Kind: KindAlbum, ID: "a1", Name: "X"}
	if !s.Toggle(it) || !s.Has(KindAlbum, "a1") || s.Items[0].Added.IsZero() {
		t.Fatal("add failed")
	}
	if s.Has(KindSong, "a1") {
		t.Fatal("kind must matter")
	}
	s.SetQuality(KindAlbum, "a1", "flac")
	if s.Items[0].Quality != "flac" {
		t.Fatal("quality")
	}
	if s.Toggle(it) || s.Has(KindAlbum, "a1") || len(s.Items) != 0 {
		t.Fatal("remove failed")
	}
	if Key(KindLibrary, "all") != "library:all" || Key(KindAlbum, "a1") != "album:a1" {
		t.Fatal("key")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "selection.json")
	s, err := Load(p)
	if err != nil || s.Version != 1 || len(s.Items) != 0 {
		t.Fatalf("missing file: %+v %v", s, err)
	}
	s.Toggle(Item{Kind: KindPlaylist, ID: "p", Name: "P", Quality: "mp3", Added: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)})
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	for _, w := range []string{`"exclude":[]`, `"version":1`, `"added":"2026-01-02T03:04:05Z"`, `"kind":"playlist"`} {
		if !strings.Contains(string(b), w) {
			t.Errorf("missing %s in %s", w, b)
		}
	}
	got, err := Load(p)
	if err != nil || len(got.Items) != 1 || got.Items[0] != s.Items[0] {
		t.Fatalf("roundtrip: %+v %v", got, err)
	}
}

func TestUnknownFieldsPreserved(t *testing.T) {
	p := filepath.Join(t.TempDir(), "selection.json")
	os.WriteFile(p, []byte(`{"version":2,"items":[],"exclude":["x"],"web":{"theme":"dark"}}`), 0o644)
	s, err := Load(p)
	if err != nil || s.Version != 2 {
		t.Fatalf("%+v %v", s, err)
	}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `"web":{"theme":"dark"}`) || !strings.Contains(string(b), `"exclude":[]`) {
		t.Fatalf("got %s", b)
	}
}
