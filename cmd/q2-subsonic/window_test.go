package main

import (
	"strconv"
	"testing"

	"github.com/andsuor/q2-subsonic/internal/q2spot"
)

// window walks one window of a list as the firmware does: the list's own arguments (base) with the
// window's cursor, then with each reply's cursor in its place while the reply says "pages". It
// gives the rows and how many replies they took.
func (d *tdaemon) window(t *testing.T, op, cursor string, base ...string) ([]q2spot.Row, int) {
	t.Helper()
	var rows []q2spot.Row
	for n := 1; ; n++ {
		args := base
		if cursor != "" {
			args = append(base[:len(base):len(base)], cursor)
		}
		r := d.do(op, args...)
		rows, cursor = append(rows, r.Rows...), r.Cursor
		if cursor == "" {
			return rows, n
		}
		if r.Message != "pages" || n > 20 {
			t.Fatalf("%s %v: reply %d has cursor %q, message %q", op, args, n, r.Cursor, r.Message)
		}
	}
}

// pageRow is rows' page row to dir ("prev" or "next"), which must be first or last.
func pageRow(t *testing.T, rows []q2spot.Row, dir string) q2spot.Row {
	t.Helper()
	i := 0
	if dir == "next" {
		i = len(rows) - 1
	}
	if r := rows[i]; r.Kind != "page" || r.URI != dir || r.Position != "" {
		t.Fatalf("row %d of %d is %+v, want a page row to %s", i, len(rows), r, dir)
	}
	return rows[i]
}

func count(rows []q2spot.Row, kind string) (n int) {
	for _, r := range rows {
		if r.Kind == kind {
			n++
		}
	}
	return n
}

// A long Albums list on Q2 Pod with "window": 480 rows in at most 10 replies, then Next page,
// which the firmware asks for in the list's place; Previous page leads back.
func TestWindowAlbums(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1000, songsPer: 1}, "pages window", true)
	d.do("toggle", "album:al0") // a Sync Now row on the first reply
	rows, n := d.window(t, "albums", "")
	if rows[0].Title != "Sync Now" || n > 10 || len(rows) != 482 || rows[1].Title != "Album 000" || rows[480].Title != "Album 479" {
		t.Fatalf("first window: %d rows in %d replies, from %q", len(rows), n, rows[0].Title)
	}
	if next := pageRow(t, rows, "next"); next.Context != "480" || next.Title != "Next page" {
		t.Fatalf("next %+v", next)
	}
	rows, n = d.window(t, "albums", "480")
	if prev := pageRow(t, rows, "prev"); prev.Context != "" || prev.Title != "Previous page" {
		t.Fatalf("prev %+v", prev)
	}
	if n > 10 || len(rows) != 482 || rows[1].Title != "Album 480" || pageRow(t, rows, "next").Context != "960" {
		t.Fatalf("second window: %d rows in %d replies, from %q", len(rows), n, rows[1].Title)
	}
	rows, _ = d.window(t, "albums", "960")
	if pageRow(t, rows, "prev").Context != "480" || len(rows) != 41 || rows[40].Title != "Album 999" {
		t.Fatalf("third window: %d rows, last %+v", len(rows), rows[len(rows)-1])
	}
	// A window asked for past a list that shrank still has its way back.
	if rows, _ = d.window(t, "albums", "1440"); len(rows) != 1 || pageRow(t, rows, "prev").Context != "960" {
		t.Fatalf("past the end: %q", titles(q2spot.Reply{Rows: rows}))
	}
}

// The same for a list held whole: a 1,000-song playlist, its own rows counted in.
func TestWindowPlaylist(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1000}, "pages window", true)
	seen := map[string]bool{}
	cursor, windows := "", 0
	for {
		rows, n := d.window(t, "playlist", cursor, "playlist:pl1")
		if cursor != "" {
			pageRow(t, rows, "prev")
		}
		if n > 10 {
			t.Fatalf("window %d took %d replies", windows, n)
		}
		for _, r := range rows {
			if r.Position == "toggle" {
				seen[r.Context] = true
			}
		}
		if windows++; rows[len(rows)-1].Kind != "page" {
			break
		}
		cursor = pageRow(t, rows, "next").Context
	}
	if len(seen) != 1000 || windows != 3 {
		t.Fatalf("%d songs in %d windows", len(seen), windows)
	}
	// Back in a shrunken list: Previous page even with nothing left to show.
	if rows := d.do("playlist", "playlist:pl1", "1920").Rows; len(rows) != 1 || pageRow(t, rows, "prev").Context != "1440" {
		t.Fatalf("past the end: %d rows", len(rows))
	}
}

// Without "window" no page row is ever sent: the firmware doesn't know the kind.
func TestNoWindowNoPageRows(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 600, songsPer: 1, artists: 200, groups: 3}, "pages sort", true)
	for _, op := range []string{"albums", "artists"} {
		r := d.do(op)
		for n := 0; ; n++ {
			if len(r.Rows) != 50 && r.Cursor != "" || count(r.Rows, "page")+count(r.Rows, "sort") != 0 {
				t.Fatalf("%s reply %d: %d rows %q", op, n, len(r.Rows), titles(r))
			}
			if r.Cursor == "" {
				break
			}
			r = d.do(op, r.Cursor)
		}
	}
	if r := d.do("artists"); r.Rows[0].URI != "artist" {
		t.Fatalf("Artists without window: %+v", r.Rows[0])
	}
}

// Artists opens on the server's index groups when there are many artists.
func TestArtistsIndex(t *testing.T) {
	d := newTestDaemon(t, &nav{artists: 60, groups: 3}, "pages window", true) // 61 artists
	r := d.do("artists")
	if len(r.Rows) != 3 || r.Cursor != "" {
		t.Fatalf("letters: %q", titles(r))
	}
	for i, want := range []q2spot.Row{list("artists", "A", "21 artists", "i:A"), list("artists", "B", "20 artists", "i:B"),
		list("artists", "C", "20 artists", "i:C")} {
		if r.Rows[i] != want {
			t.Errorf("row %d: %+v, want %+v", i, r.Rows[i], want)
		}
	}
	rows, _ := d.window(t, "artists", "", "i:B")
	if len(rows) != 20 || rows[0].Title != "Artist 00002" || rows[1].Title != "Artist 00005" || rows[0].URI != "artist" {
		t.Fatalf("B: %d rows from %+v", len(rows), rows[0])
	}
	if rows, _ := d.window(t, "artists", "", "i:Z"); len(rows) != 1 || rows[0].Kind != "info" {
		t.Fatalf("an unknown letter: %+v", rows)
	}

	// 60 artists, or one group, or unnamed groups: one list, paged with a cursor alone.
	for name, n := range map[string]*nav{"60": {artists: 59, groups: 3}, "one group": {artists: 100, groups: 1}, "unnamed": {artists: 100}} {
		d := newTestDaemon(t, n, "pages window", true)
		r := d.do("artists")
		if len(r.Rows) != 48 || r.Rows[0].URI != "artist" || r.Cursor != "48" {
			t.Fatalf("%s: %d rows from %+v, cursor %q", name, len(r.Rows), r.Rows[0], r.Cursor)
		}
		if r = d.do("artists", "48"); r.Rows[0].URI != "artist" || len(r.Rows) != min(n.artists+1-48, 48) {
			t.Fatalf("%s, second reply: %d rows from %+v", name, len(r.Rows), r.Rows[0])
		}
	}
}

// Albums has a Sort row on Q2 Pod with "sort" (and "window"); its orders are the server's.
func TestAlbumsSort(t *testing.T) {
	n := &nav{albums: 100, songsPer: 1}
	if r := newTestDaemon(t, n, "pages window", true).do("albums"); count(r.Rows, "sort") != 0 {
		t.Fatalf("a Sort row without the sort word: %q", titles(r))
	}
	d := newTestDaemon(t, n, "pages window sort", true)
	d.do("toggle", "album:al0")
	r := d.do("albums")
	want := q2spot.Row{Kind: "sort", URI: "albumsort", Title: "Sort"}
	if len(r.Rows) != 49 || r.Rows[0].Title != "Sync Now" || r.Rows[1] != want || r.Rows[48].Title != "Album 046" || r.Cursor != "48" {
		t.Fatalf("first reply: %d rows %q, cursor %q", len(r.Rows), titles(r), r.Cursor)
	}
	rows, _ := d.window(t, "albums", "")
	if len(rows) != 102 || count(rows, "sort") != 1 || rows[101].Title != "Album 099" {
		t.Fatalf("albums: %d rows, %d Sort rows", len(rows), count(rows, "sort"))
	}
	r = d.do("albumsort")
	for i, w := range []q2spot.Row{list("albums", "Recently Added", "", "newest"), list("albums", "By Artist", "", "artist"),
		list("albums", "By Year", "", "year")} {
		if len(r.Rows) != 3 || r.Rows[i] != w {
			t.Fatalf("albumsort: %+v", r.Rows)
		}
	}
	for key, typ := range map[string]string{"newest": "newest", "artist": "alphabeticalByArtist", "year": "byYear"} {
		rows, _ := d.window(t, "albums", "", key)
		q := n.query("getAlbumList2")
		if len(rows) != 101 || count(rows, "sort") != 0 || q.Get("type") != typ || q.Get("offset") != strconv.Itoa(96) {
			t.Fatalf("%s: %d rows, asked type %q offset %q", key, len(rows), q.Get("type"), q.Get("offset"))
		}
		if from, sub := q.Get("fromYear"), rows[2].Subtitle; (key == "year") != (from == "9999" && q.Get("toYear") == "0") ||
			(key == "year") != (sub == "2020 · Artist") {
			t.Fatalf("%s: fromYear %q, subtitle %q", key, from, sub)
		}
	}
	if r := d.do("albums", "nonsense"); r.Status != q2spot.Bad {
		t.Fatalf("an unknown order: %+v", r)
	}
}
