package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/q2spot"
)

// On Q2 Pod with "sections" the root names the tabs: list rows, op menu, each with an argument (a
// section answering "sections" again would be a plain list of the tabs).
func TestTabsRoot(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "sections", true)
	r := d.do("menu")
	if got := strings.Join(titles(r), "|"); r.Message != "sections" || got != "Home|Discover|Playlists|Albums|Artists" {
		t.Fatalf("message %q, tabs %q", r.Message, got)
	}
	for _, row := range r.Rows {
		if row.Kind != "list" || row.URI != "menu" || row.Context == "" {
			t.Errorf("tab %+v", row)
		}
		if got := d.do("menu", row.Context); got.Message == "sections" || got.Status != "" || len(got.Rows) < 2 ||
			got.Rows[0].Kind != "search" {
			t.Errorf("%s: message %q, status %q, rows %q", row.Title, got.Message, got.Status, titles(got))
		}
	}
}

func TestTabsHome(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2, mediaErr: 70}, "sections", true)
	r := d.do("menu", "home")
	if got := strings.Join(titles(r), "|"); r.Message != "refresh" || r.Rows[0].Kind != "search" ||
		got != "Search|Sync Entire Library|Sync|On This Q2|Playlists|Albums|Artists|Discover|Settings" {
		t.Fatalf("message %q, rows %q", r.Message, got)
	}
	d.stream.backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	d.do("play", "album:al0 0")
	eventually(t, "the stream error", func() bool { r = d.do("menu", "home"); _, ok := find(r, "Couldn't stream Song 0"); return ok })
	if n := len(r.Rows); n != 11 || r.Rows[n-1].Title != "Details" || r.Rows[0].Kind != "search" {
		t.Fatalf("rows %q", titles(r))
	}
}

// tab walks a tab as the firmware does: menu with the tab's name, then with each cursor after it.
func (d *tdaemon) tab(t *testing.T, name string) (first q2spot.Reply, rows []q2spot.Row) {
	t.Helper()
	first = d.do("menu", name)
	for r, n := first, 0; ; n++ {
		if len(r.Rows) > q2spot.MaxRows || n > 20 {
			t.Fatalf("reply %d: %d rows", n, len(r.Rows))
		}
		rows = append(rows, r.Rows...)
		if r.Cursor == "" {
			return first, rows
		}
		r = d.do("menu", name, r.Cursor)
	}
}

func albumsIn(rows []q2spot.Row) (n int, once bool) {
	seen := map[string]bool{}
	for _, r := range rows {
		if r.URI == "albumart" {
			seen[r.Context] = true
			n++
		}
	}
	return n, n == len(seen)
}

// A tab's list is led by Search (the title bar's glass), then Sync Now when due; nothing is lost
// to the rows they push on, and no later page has them.
func TestTabsAlbums(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 120, songsPer: 1}, "pages sections", true)
	first, rows := d.tab(t, "albums")
	if first.Rows[0].Kind != "search" || first.Rows[1].Title != "Album 000" || first.Message != "pages" {
		t.Fatalf("first page %q, message %q", titles(first)[:2], first.Message)
	}
	if n, once := albumsIn(rows); n != 120 || !once || count(rows, "search") != 1 {
		t.Fatalf("%d albums, each once %v, %d Search rows", n, once, count(rows, "search"))
	}
	d.do("toggle", "album:al0")
	first, rows = d.tab(t, "albums")
	if first.Rows[0].Kind != "search" || first.Rows[1].Title != "Sync Now" || len(first.Rows) != 50 || first.Cursor != "48" {
		t.Fatalf("first page %q, %d rows, cursor %q", titles(first)[:2], len(first.Rows), first.Cursor)
	}
	if n, once := albumsIn(rows); n != 120 || !once || count(rows, "search") != 1 {
		t.Fatalf("with Sync Now: %d albums, each once %v", n, once)
	}
	for _, name := range []string{"playlists", "artists", "discover"} {
		if r := d.do("menu", name); r.Rows[0].Kind != "search" || r.Status != "" {
			t.Errorf("%s: %q", name, titles(r))
		}
	}
	if r := d.do("menu", "discover"); r.Rows[1].Title != "Shuffle All" || len(r.Rows) != 8 {
		t.Errorf("discover: %q", titles(r))
	}
	// Opened from a row, not as a tab, a list is as before.
	if r := d.do("albums"); r.Rows[0].Title != "Sync Now" || count(r.Rows, "search") != 0 {
		t.Errorf("albums: %q", titles(r)[:2])
	}
}

// A list one row short of a full reply keeps its last album when Search and Sync Now lead it.
func TestTabsLeadBoundary(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 49, songsPer: 1}, "pages sections", true)
	d.do("toggle", "album:al0")
	r := d.do("menu", "albums")
	if len(r.Rows) != 50 || r.Cursor != "48" {
		t.Fatalf("%d rows, cursor %q", len(r.Rows), r.Cursor)
	}
	if r = d.do("menu", "albums", r.Cursor); len(r.Rows) != 1 || r.Rows[0].Title != "Album 048" || r.Cursor != "" {
		t.Fatalf("second page %q, cursor %q", titles(r), r.Cursor)
	}
}

// With "window" a tab's every window is led by Search, before Previous page: the firmware takes a
// leading Search to be in each one when it picks the row Next page selects, and the glass stays.
func TestTabsWindows(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1000, songsPer: 1}, "pages window sort sections", true)
	d.do("toggle", "album:al0")
	rows, n := d.window(t, "menu", "", "albums")
	if got := strings.Join(titles(q2spot.Reply{Rows: rows[:4]}), "|"); got != "Search|Sync Now|Sort|Album 000" || n > 10 {
		t.Fatalf("first window starts %q, %d replies", got, n)
	}
	if na, once := albumsIn(rows); na != 479 || !once || pageRow(t, rows, "next").Context != "480" {
		t.Fatalf("first window: %d albums, each once %v", na, once)
	}
	rows, n = d.window(t, "menu", "480", "albums")
	if rows[0].Kind != "search" || rows[1].Kind != "page" || rows[1].URI != "prev" || rows[2].Title != "Album 479" ||
		count(rows, "search") != 1 || n > 10 {
		t.Fatalf("second window starts %q, %d replies", titles(q2spot.Reply{Rows: rows[:3]}), n)
	}
	if r := d.do("menu", "albums", "528"); count(r.Rows, "search") != 0 {
		t.Errorf("a window's later reply has Search: %q", titles(r)[:2])
	}
}

// Signed out, every tab is the sign-in row (the tabs outlive Sign Out), never the tabs or a failure.
func TestTabsSignedOut(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "sections", false)
	for _, args := range [][]string{nil, {"home"}, {"albums"}, {"discover"}} {
		r := d.do("menu", args...)
		if r.Message != "refresh" || r.Status != "" || len(r.Rows) != 1 || r.Rows[0].URI != "signin" {
			t.Errorf("menu %v: %+v", args, r)
		}
	}
}

// A tab other than Home shown first after signing in clears the QR page's state, as Home does.
func TestTabsClearSigninState(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "sections", true)
	file := filepath.Join(d.tmp, "q2-subsonic.state")
	d.state.Write(map[string]string{"note": "Signed in"})
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), "note=") {
		t.Fatalf("state %q", b)
	}
	d.do("menu", "albums")
	if b, _ := os.ReadFile(file); strings.Contains(string(b), "note=") {
		t.Errorf("state kept %q", b)
	}
}

// Without "sections" the root is one list whatever the argument.
func TestNoTabsOnOlderFirmware(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 1}, "pages window sort", true)
	want := strings.Join(titles(d.do("menu")), "|")
	if r := d.do("menu", "albums"); r.Message != "refresh" || strings.Join(titles(r), "|") != want || !strings.Contains(want, "|Discover|Search|Settings") {
		t.Errorf("menu albums: %q, message %q", titles(r), r.Message)
	}
	if r := d.do("albums"); count(r.Rows, "search") != 0 {
		t.Errorf("albums: %q", titles(r))
	}
}
