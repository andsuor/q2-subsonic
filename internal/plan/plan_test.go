package plan

import (
	"reflect"
	"strings"
	"testing"

	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/selection"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

const root = "/mnt/mmc"

func song(id, title string) subsonic.Song {
	return subsonic.Song{ID: id, Title: title, Album: "Alb", AlbumID: "a1", AlbumArtist: "Art", Year: 2020,
		Disc: 1, Track: 1, Duration: 100, Size: 1000, Suffix: "flac"}
}

func item(kind selection.Kind, id, q string) selection.Item {
	return selection.Item{Kind: kind, ID: id, Quality: q}
}

func entry(s subsonic.Song, q string, size int64, items ...string) *library.Entry {
	return &library.Entry{ID: s.ID, Path: library.SongPath(root, s, q, false), Quality: q, Size: size,
		Items: items, Meta: library.MetaKey(s, q)}
}

func manifest(es ...*library.Entry) *library.Manifest {
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	for _, e := range es {
		m.Songs[e.ID] = e
	}
	return m
}

const big = int64(1) << 40

func TestPlanDuplicatesAcrossItems(t *testing.T) {
	s := song("s1", "One")
	sel := selection.Set{Items: []selection.Item{item("album", "a1", "original"), item("playlist", "p1", "original")}}
	res := map[string][]subsonic.Song{"album:a1": {s}, "playlist:p1": {s, s}}
	p := Compute(sel, res, map[string]string{"playlist:p1": "Mix"}, manifest(), root, big)
	if len(p.Add) != 1 || !reflect.DeepEqual(p.Add[0].Items, []string{"album:a1", "playlist:p1"}) {
		t.Fatalf("add = %+v", p.Add)
	}
	if got := p.Playlists["Mix"]; len(got) != 2 || got[0] != p.Add[0].Path {
		t.Fatalf("playlist = %v", got)
	}
}

func TestPlanBestQualityWins(t *testing.T) {
	s := song("s1", "One")
	sel := selection.Set{Items: []selection.Item{item("album", "a1", "mp3-128"), item("song", "s1", "")}}
	res := map[string][]subsonic.Song{"album:a1": {s}, "song:s1": {s}}
	p := Compute(sel, res, nil, manifest(), root, big)
	if len(p.Add) != 1 || p.Add[0].Quality != "original" || p.Add[0].Bytes != 1000 {
		t.Fatalf("add = %+v", p.Add)
	}
	if !Better("original", "mp3-320") || !Better("mp3-320", "mp3-128") || Better("mp3-128", "mp3-128") {
		t.Fatal("ranking")
	}
}

func TestPlanQualityChangeReplaces(t *testing.T) {
	s := song("s1", "One")
	sel := selection.Set{Items: []selection.Item{item("song", "s1", "mp3-320")}}
	m := manifest(entry(s, "original", 5000, "song:s1"))
	p := Compute(sel, map[string][]subsonic.Song{"song:s1": {s}}, nil, m, root, big)
	if len(p.Replace) != 1 || len(p.Add) != 0 || p.Replace[0].Bytes != 100*320*1000/8 {
		t.Fatalf("plan = %+v", p)
	}
	if p.NeedBytes != 4000000-5000 {
		t.Fatalf("need = %d", p.NeedBytes)
	}
}

func TestPlanRemovedItemDeletes(t *testing.T) {
	a, b := song("s1", "One"), song("s2", "Two")
	sel := selection.Set{Items: []selection.Item{item("song", "s2", "original")}}
	m := manifest(entry(a, "original", 1000, "song:s1"), entry(b, "original", 1000, "song:s2"))
	p := Compute(sel, map[string][]subsonic.Song{"song:s2": {b}}, nil, m, root, big)
	if len(p.Remove) != 1 || p.Remove[0].ID != "s1" || len(p.Add)+len(p.Replace)+len(p.Rename) != 0 {
		t.Fatalf("plan = %+v", p)
	}
}

func TestPlanArtistNewAlbum(t *testing.T) {
	a, b := song("s1", "One"), song("s2", "Two")
	b.AlbumID, b.Album = "a2", "Other"
	sel := selection.Set{Items: []selection.Item{item("artist", "ar1", "original")}}
	m := manifest(entry(a, "original", 1000, "artist:ar1"))
	p := Compute(sel, map[string][]subsonic.Song{"artist:ar1": {a, b}}, nil, m, root, big)
	if len(p.Add) != 1 || p.Add[0].Song.ID != "s2" || len(p.Remove) != 0 {
		t.Fatalf("plan = %+v", p)
	}
}

func TestPlanRenameOnMetadataChange(t *testing.T) {
	old, s := song("s1", "One"), song("s1", "Uno")
	sel := selection.Set{Items: []selection.Item{item("song", "s1", "original")}}
	m := manifest(entry(old, "original", 1000, "song:s1"))
	p := Compute(sel, map[string][]subsonic.Song{"song:s1": {s}}, nil, m, root, big)
	if len(p.Rename) != 1 || len(p.Add) != 0 || p.Rename[0].From != m.Songs["s1"].Path ||
		p.Rename[0].To != library.SongPath(root, s, "original", false) || p.Rename[0].ID != "s1" {
		t.Fatalf("plan = %+v", p)
	}
}

func TestPlanNotEnoughSpace(t *testing.T) {
	a, b := song("s1", "One"), song("s2", "Two")
	sel := selection.Set{Items: []selection.Item{item("album", "a1", "original")}}
	p := Compute(sel, map[string][]subsonic.Song{"album:a1": {a, b}}, nil, manifest(), root, 16<<20+1999)
	if p.Fits || p.NeedBytes != 2000 || p.FreeBytes != 16<<20+1999 {
		t.Fatalf("plan = %+v", p)
	}
	if !Compute(sel, map[string][]subsonic.Song{"album:a1": {a, b}}, nil, manifest(), root, 16<<20+2000).Fits {
		t.Fatal("should fit exactly")
	}
}

func TestPlanUpToDate(t *testing.T) {
	s := song("s1", "One")
	sel := selection.Set{Items: []selection.Item{item("song", "s1", "original")}}
	m := manifest(entry(s, "original", 1000, "song:s1"))
	p := Compute(sel, map[string][]subsonic.Song{"song:s1": {s}}, nil, m, root, big)
	if len(p.Add)+len(p.Replace)+len(p.Rename)+len(p.Remove) != 0 || p.NeedBytes != 0 || !p.Fits {
		t.Fatalf("plan = %+v", p)
	}
}

func TestPlanCollisionGetsID(t *testing.T) {
	a, b := song("s1", "Same"), song("s2", "Same")
	sel := selection.Set{Items: []selection.Item{item("album", "a1", "original")}}
	p := Compute(sel, map[string][]subsonic.Song{"album:a1": {b, a}}, nil, manifest(), root, big)
	if len(p.Add) != 2 || p.Add[0].Path == p.Add[1].Path {
		t.Fatalf("add = %+v", p.Add)
	}
	for _, w := range p.Add {
		if (w.Song.ID == "s2") != (w.Path == library.WithID(library.SongPath(root, b, "original", false), "s2")) {
			t.Fatalf("wrong song suffixed: %+v", w)
		}
	}
}

// An original whose suffix is empty or unsafe is fetched as mp3-320, so the .mp3 it is saved as
// really holds MP3 (never FLAC/OGG bytes under a .mp3 name).
func TestPlanUnsafeSuffixTranscodes(t *testing.T) {
	ok, empty, bad := song("s1", "One"), song("s2", "Two"), song("s3", "Three")
	empty.Suffix, bad.Suffix = "", "dsdiff"
	sel := selection.Set{Items: []selection.Item{item("album", "a1", "original")}}
	p := Compute(sel, map[string][]subsonic.Song{"album:a1": {ok, empty, bad}}, nil, manifest(), root, big)
	got := map[string]string{}
	for _, w := range p.Add {
		got[w.Song.ID] = w.Quality
		if w.Path != library.SongPath(root, w.Song, w.Quality, false) {
			t.Errorf("%s: path %s", w.Song.ID, w.Path)
		}
	}
	if want := map[string]string{"s1": "original", "s2": "mp3-320", "s3": "mp3-320"}; !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

// A song synced under a name SafeName no longer makes (a leading "." the scan hides, or ".." the
// firmware refuses) is moved to its clean path though its metadata is unchanged (I18, M40).
func TestPlanRepathsDottedNames(t *testing.T) {
	s := song("s1", "One")
	s.AlbumArtist = ".38 Special"
	old := library.Entry{ID: "s1", Path: root + "/Subsonic/.38 Special/2020 - Alb/01 - One.flac", Quality: "original",
		Size: 1000, Items: []string{"song:s1"}, Meta: library.MetaKey(s, "original")}
	sel := selection.Set{Items: []selection.Item{item("song", "s1", "original")}}
	p := Compute(sel, map[string][]subsonic.Song{"song:s1": {s}}, nil, manifest(&old), root, big)
	if len(p.Rename) != 1 || p.Rename[0].To != root+"/Subsonic/_38 Special/2020 - Alb/01 - One.flac" {
		t.Fatalf("plan = %+v", p)
	}
}

// The card is FAT/exFAT: paths that differ only in case are one file. A new song whose path is a
// case variant of a removed song's gets its [id], so neither deletes the other (I5).
func TestPlanCaseOnlyCollisionGetsID(t *testing.T) {
	gone, s := song("s1", "One"), song("s2", "One")
	gone.AlbumArtist = "Abba"
	s.AlbumArtist = "ABBA"
	sel := selection.Set{Items: []selection.Item{item("song", "s2", "original")}}
	p := Compute(sel, map[string][]subsonic.Song{"song:s2": {s}}, nil, manifest(entry(gone, "original", 1000)), root, big)
	if len(p.Add) != 1 || p.Add[0].Path != library.WithID(library.SongPath(root, s, "original", false), "s2") {
		t.Fatalf("plan = %+v", p)
	}
}

// A new song never plans onto the path of a song that moves away in the same sync: the move's
// remove of its old file would delete it (M16).
func TestPlanNoAddOntoMovingPath(t *testing.T) {
	x, y := song("x", "T"), song("y", "T")
	sel := selection.Set{Items: []selection.Item{item("song", "x", "original"), item("song", "y", "mp3-320")}}
	m := manifest(entry(x, "mp3-320", 1000, "song:x")) // x goes from .mp3 to .flac; y wants x's .mp3 path
	p := Compute(sel, map[string][]subsonic.Song{"song:x": {x}, "song:y": {y}}, nil, m, root, big)
	for _, w := range p.Add {
		if w.Song.ID == "y" && strings.EqualFold(w.Path, m.Songs["x"].Path) {
			t.Fatalf("y planned onto x's old path %s", w.Path)
		}
	}
}

// Two songs whose tags were swapped never move onto each other's current file (I6).
func TestPlanSwapNeverCrossesPaths(t *testing.T) {
	x, y := song("x", "Alpha"), song("y", "Beta")
	x.Track, y.Track = 1, 2
	ex, ey := entry(x, "original", 1000, "album:a1"), entry(y, "original", 1000, "album:a1")
	x.Title, x.Track, y.Title, y.Track = "Beta", 2, "Alpha", 1
	sel := selection.Set{Items: []selection.Item{item("album", "a1", "original")}}
	p := Compute(sel, map[string][]subsonic.Song{"album:a1": {x, y}}, nil, manifest(ex, ey), root, big)
	if len(p.Rename) != 2 {
		t.Fatalf("plan = %+v", p)
	}
	for _, mv := range p.Rename {
		for _, e := range []*library.Entry{ex, ey} {
			if e.ID != mv.ID && strings.EqualFold(mv.To, e.Path) {
				t.Errorf("%s moves onto %s's file %s", mv.ID, e.ID, e.Path)
			}
		}
	}
}

// A new format (same id) is fetched again, never renamed: the old bytes under a .flac name would
// stay forever (I7). So is an original whose server size changed (a re-rip).
func TestPlanFormatOrSizeChangeReplaces(t *testing.T) {
	old, s := song("s1", "One"), song("s1", "One")
	old.Suffix = "mp3"
	sel := selection.Set{Items: []selection.Item{item("song", "s1", "original")}}
	p := Compute(sel, map[string][]subsonic.Song{"song:s1": {s}}, nil, manifest(entry(old, "original", 900, "song:s1")), root, big)
	if len(p.Replace) != 1 || len(p.Rename) != 0 || !strings.HasSuffix(p.Replace[0].Path, ".flac") {
		t.Fatalf("format: plan = %+v", p)
	}
	e := entry(s, "original", 1000, "song:s1")
	e.SrcSize = 1000
	s.Size = 2000
	p = Compute(sel, map[string][]subsonic.Song{"song:s1": {s}}, nil, manifest(e), root, big)
	if len(p.Replace) != 1 || p.Replace[0].Path != e.Path || p.NeedBytes != 1000 {
		t.Fatalf("size: plan = %+v", p)
	}
}
