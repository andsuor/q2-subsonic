package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/q2spot"
)

// rows is every row of a list, asked for page by page as the firmware does.
func (d *tdaemon) rows(op string, args ...string) []q2spot.Row {
	var out []q2spot.Row
	for cur := ""; ; {
		r := d.do(op, append(args[:len(args):len(args)], cur)...)
		out = append(out, r.Rows...)
		if cur = r.Cursor; cur == "" {
			return out
		}
	}
}

// queued is how many songs the running stream's queue file holds, -1 while there is none.
func (d *tdaemon) queued() int {
	m, _ := filepath.Glob(filepath.Join(d.card, ".subsonic", "stream", "queue-*.m3u"))
	if len(m) != 1 {
		return -1
	}
	b, _ := os.ReadFile(m[0])
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			n++
		}
	}
	return n
}

func (d *tdaemon) waitQueued(t *testing.T, want int) {
	t.Helper()
	eventually(t, "a queue of "+plural(want, "song"), func() bool { return d.queued() == want })
}

func TestDiscoverRows(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	r := d.do("discover")
	want := "action shuffle Shuffle All |list albumlist Recently Added newest|list albumlist Recently Played recent|" +
		"list albumlist Most Played frequent|list albumlist Random Albums random|list starred Starred |list genres Genres "
	var got []string
	for _, row := range r.Rows {
		got = append(got, strings.Join([]string{row.Kind, row.URI, row.Title, row.Context}, " "))
	}
	if strings.Join(got, "|") != want {
		t.Fatalf("rows %q", got)
	}
	if row, ok := find(d.do("menu"), "Discover"); !ok || row.Kind != "list" || row.URI != "discover" {
		t.Fatalf("menu row %+v", row)
	}
}

// A server-made album list sends its type and is paged by the server's offset.
func TestAlbumListPaged(t *testing.T) {
	n := &nav{albums: 120, songsPer: 1}
	d := newTestDaemon(t, n, "", true)
	for _, typ := range []string{"newest", "recent", "frequent"} {
		r := d.do("albumlist", typ)
		if q := n.query("getAlbumList2"); len(r.Rows) != 50 || r.Cursor != "50" || q.Get("type") != typ || q.Get("offset") != "0" {
			t.Fatalf("%s: %d rows, cursor %q, query %v", typ, len(r.Rows), r.Cursor, q)
		}
		if r.Rows[0].URI != "album" || r.Rows[0].Context != "album:al0" || r.Rows[0].Position != "toggle" {
			t.Fatalf("%s: row %+v", typ, r.Rows[0])
		}
		r = d.do("albumlist", typ, "100")
		if q := n.query("getAlbumList2"); len(r.Rows) != 20 || r.Cursor != "" || q.Get("offset") != "100" {
			t.Fatalf("%s page 3: %d rows, cursor %q, query %v", typ, len(r.Rows), r.Cursor, q)
		}
	}
	if r := d.do("albumlist", "alphabeticalByName"); r.Status != q2spot.Bad {
		t.Fatalf("an unknown list: %+v", r)
	}
}

// Random Albums is asked for once and stays put over Back, a hold and Sync Now's paging; opening
// Discover again draws a new one.
func TestRandomAlbumsKept(t *testing.T) {
	n := &nav{albums: 120, songsPer: 1}
	d := newTestDaemon(t, n, "", true)
	if r := d.do("albumlist", "random"); len(r.Rows) != 50 || r.Cursor != "" {
		t.Fatalf("%d rows, cursor %q", len(r.Rows), r.Cursor)
	}
	if q := n.query("getAlbumList2"); q.Get("type") != "random" || q.Get("size") != "50" {
		t.Fatalf("query %v", q)
	}
	d.do("toggle", "album:al3") // a hold: Sync Now takes the first row, the last goes to page two
	rows := d.rows("albumlist", "random")
	if len(rows) != 51 || rows[0].Title != "Sync Now" || rows[50].Context != "album:al49" {
		t.Fatalf("%d rows, first %q, last %+v", len(rows), rows[0].Title, rows[len(rows)-1])
	}
	if got := n.reqs("getAlbumList2"); got != 1 {
		t.Fatalf("%d requests, want 1", got)
	}
	d.do("discover")
	d.do("albumlist", "random")
	if got := n.reqs("getAlbumList2"); got != 2 {
		t.Fatalf("after Discover: %d requests, want 2", got)
	}
}

func TestGenres(t *testing.T) {
	n := &nav{albums: 120, songsPer: 1}
	d := newTestDaemon(t, n, "", true)
	r := d.do("genres")
	var got []string
	for _, row := range r.Rows {
		got = append(got, strings.Join([]string{row.Kind, row.URI, row.Title, row.Subtitle, row.Context}, " "))
	}
	if want := "list genre ambient  ambient|list genre Hip Hop: Old 1 album Hip Hop: Old|list genre Rock 2 albums Rock"; strings.Join(got, "|") != want {
		t.Fatalf("rows %q", got)
	}
	// A genre is its Shuffle, then its albums: none skipped or shown twice over the pages, with and
	// without a Sync Now row.
	for _, waiting := range []bool{false, true} {
		if waiting {
			d.do("toggle", "album:al3")
		}
		rows := d.rows("genre", "Hip Hop: Old")
		if waiting {
			if rows[0].Title != "Sync Now" {
				t.Fatalf("first row %q", rows[0].Title)
			}
			rows = rows[1:]
		}
		if rows[0].Kind != "action" || rows[0].URI != "shuffle" || rows[0].Title != "Shuffle" || rows[0].Context != "genre:Hip Hop: Old" {
			t.Fatalf("first row %+v", rows[0])
		}
		seen := map[string]bool{}
		for _, row := range rows[1:] {
			if row.URI != "album" || seen[row.Context] {
				t.Fatalf("row %+v (shown twice: %v)", row, seen[row.Context])
			}
			seen[row.Context] = true
		}
		if len(seen) != 120 {
			t.Fatalf("waiting %v: %d albums, want 120", waiting, len(seen))
		}
	}
	if q := n.query("getAlbumList2"); q.Get("type") != "byGenre" || q.Get("genre") != "Hip Hop: Old" {
		t.Fatalf("query %v", q)
	}
}

func TestStarred(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 2}, "", true)
	r := d.do("starred")
	var got []string
	for _, row := range r.Rows {
		got = append(got, strings.Join([]string{row.Kind, row.URI, row.Title, row.Context}, " "))
	}
	want := "list artist Artist artist:ar1|list album Album 000 album:al0|action play Song 0 song:al1.0 starred: 0|action play Song 1 song:al1.1 starred: 1"
	if strings.Join(got, "|") != want {
		t.Fatalf("rows %q", got)
	}
}

// Starred has no bound, so its songs live in the one-minute cache only: a song pressed plays on
// through the list, and nothing holds the list once the cache dropped it.
func TestStarredNotKept(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 2}, "playlist", true)
	r := d.do("starred")
	d.browseTTL = 0
	d.tick()
	d.mu.Lock()
	kept := len(d.cache) + len(d.listed.songs)
	d.mu.Unlock()
	if kept != 0 {
		t.Fatalf("starred songs still held after the cache dropped them: %d", kept)
	}
	if r := d.do("play", r.Rows[2].Context); r.Message != "Playing Song 0" {
		t.Fatalf("play: %+v", r)
	}
	d.waitQueued(t, 2)
}

func TestShuffle(t *testing.T) {
	n := &nav{albums: 2, songsPer: 3}
	d := newTestDaemon(t, n, "playlist", true)
	if r := d.do("shuffle", ""); r.Status != "" || r.Message != "Playing Song 0" {
		t.Fatalf("shuffle all: %+v", r)
	}
	if q := n.query("getRandomSongs"); q.Get("size") != "100" || q.Has("genre") {
		t.Fatalf("query %v", q)
	}
	d.waitQueued(t, 3)
	if !exists(filepath.Join(d.tmp, "q2-subsonic.play")) {
		t.Fatal("no .play")
	}
	if r := d.do("shuffle", "genre:Hip Hop: Old"); r.Message != "Playing Song 0" {
		t.Fatalf("shuffle a genre: %+v", r)
	}
	if q := n.query("getRandomSongs"); q.Get("genre") != "Hip Hop: Old" {
		t.Fatalf("query %v", q)
	}
	n.empty = true
	if r := d.do("shuffle", ""); r.Status != q2spot.Unavailable || r.Message != "Nothing to play" {
		t.Fatalf("nothing to shuffle: %+v", r)
	}
}

func TestRadio(t *testing.T) {
	n := &nav{albums: 2, songsPer: 3}
	d := newTestDaemon(t, n, "playlist", true)
	if r := d.do("radio", "artist:ar1"); r.Status != "" || r.Message != "Playing Song 0" {
		t.Fatalf("radio: %+v", r)
	}
	if q := n.query("getSimilarSongs2"); q.Get("id") != "ar1" || q.Get("count") != "50" {
		t.Fatalf("query %v", q)
	}
	d.waitQueued(t, 3)
	for _, c := range []struct {
		n    *nav
		want string
	}{
		{&nav{noLastfm: true}, "Not available on this server"},
		{&nav{simCode: 70}, "No similar songs found"},
		{&nav{empty: true}, "No similar songs found"},
	} {
		d := newTestDaemon(t, c.n, "", true)
		if r := d.do("radio", "artist:ar1"); r.Status != q2spot.Unavailable || r.Message != c.want {
			t.Errorf("want %q: %+v", c.want, r)
		}
		if d.stream.active() {
			t.Errorf("%q: a stream started", c.want)
		}
	}
	if r := newTestDaemon(t, &nav{simCode: 50}, "", true).do("radio", "artist:ar1"); r.Status != q2spot.Error || r.Message != "data not found" {
		t.Errorf("another error keeps its message: %+v", r)
	}
}

// Top Songs lists an artist's most played songs; one pressed plays on through the list.
func TestTopSongs(t *testing.T) {
	n := &nav{albums: 2, songsPer: 3}
	d := newTestDaemon(t, n, "playlist", true)
	r := d.do("top", "artist:ar1")
	if q := n.query("getTopSongs"); q.Get("artist") != "Artist" || q.Get("count") != "50" {
		t.Fatalf("query %v", q)
	}
	if len(r.Rows) != 3 || r.Rows[1].URI != "play" || r.Rows[1].Context != "song:al0.1 top:ar1 1" || r.Rows[1].Position != "toggle" {
		t.Fatalf("rows %+v", r.Rows)
	}
	if r := d.do("play", r.Rows[1].Context); r.Message != "Playing Song 1" {
		t.Fatalf("play: %+v", r)
	}
	d.waitQueued(t, 2)
	d = newTestDaemon(t, &nav{noLastfm: true}, "", true)
	if r := d.do("top", "artist:ar1"); len(r.Rows) != 1 || r.Rows[0].Kind != "info" || r.Rows[0].Title != "Not available on this server" {
		t.Fatalf("no Top Songs on the server: %+v", r)
	}
}

func TestArtistPageRows(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 3}, "", true)
	r := d.do("artist", "artist:ar1")
	var got []string
	for _, row := range r.Rows {
		got = append(got, strings.Join([]string{row.Kind, row.URI, row.Title, row.Context}, " "))
	}
	want := "action toggle Add to Sync artist:ar1|action play Play artist:ar1 0|action shuffle Shuffle artist:ar1|" +
		"action radio Radio artist:ar1|list top Top Songs artist:ar1|list album Album 000 album:al0|list album Album 001 album:al1"
	if strings.Join(got, "|") != want {
		t.Fatalf("rows %q", got)
	}
}

// An artist's Play answers before its albums are fetched, then plays them all; Shuffle too.
func TestArtistPlay(t *testing.T) {
	n := &nav{albums: 2, songsPer: 3, holdAlbum: make(chan struct{})}
	d := newTestDaemon(t, n, "playlist", true)
	start := time.Now()
	if r := d.do("play", "artist:ar1 0"); r.Status != "" || r.Message != "Playing Artist" {
		t.Fatalf("play: %+v", r)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the reply waited %v for the albums", took)
	}
	if d.stream.active() {
		t.Fatal("playing before the albums came")
	}
	close(n.holdAlbum)
	d.waitQueued(t, 6)
	if r := d.do("shuffle", "artist:ar1"); r.Message != "Shuffling Artist" {
		t.Fatalf("shuffle: %+v", r)
	}
	d.bg.Wait()
	d.waitQueued(t, 6)
}

// An album that fails while an artist's songs are fetched shows as the stream's error on the root.
func TestArtistPlayFails(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 3, failID: "al1", failCode: 50}, "", true)
	d.do("play", "artist:ar1 0")
	d.bg.Wait()
	r := d.do("menu")
	if _, ok := find(r, "Couldn't stream Artist"); !ok || r.Rows[len(r.Rows)-1].Title != "Details" {
		t.Fatalf("rows %q", titles(r))
	}
	if got := strings.Join(titles(d.do("details", r.Rows[len(r.Rows)-1].Context)), "|"); !strings.Contains(got, "Op: getAlbum") || !strings.Contains(got, "Code: 50") {
		t.Errorf("details %q", got)
	}
}

// Something else played while an artist's albums were still fetched wins: the artist neither starts
// over it nor leaves its error.
func TestArtistPlaySuperseded(t *testing.T) {
	n := &nav{albums: 2, songsPer: 3, holdAlbum: make(chan struct{}), failID: "al1", failCode: 50}
	d := newTestDaemon(t, n, "playlist", true)
	d.do("play", "artist:ar1 0")
	if r := d.do("shuffle", ""); r.Message != "Playing Song 0" {
		t.Fatalf("shuffle: %+v", r)
	}
	close(n.holdAlbum)
	d.bg.Wait()
	if d.queued() != 3 || d.stream.Err() != nil {
		t.Fatalf("queue of %d, error %v", d.queued(), d.stream.Err())
	}
}

// A song pressed in search results plays on through the songs found; a row from an older answer
// plays alone.
func TestSearchPlaysOn(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 3}, "playlist", true)
	row, _ := find(d.do("search", "x"), "Song 1")
	if row.Context != "song:al0.1 q: 1" {
		t.Fatalf("row %+v", row)
	}
	if r := d.do("play", row.Context); r.Message != "Playing Song 1" {
		t.Fatalf("play: %+v", r)
	}
	d.waitQueued(t, 2)
	if r := d.do("play", "song:al1.2 q: 1"); r.Message != "Playing Song 2" {
		t.Fatalf("a stale row: %+v", r)
	}
	d.bg.Wait()
	d.waitQueued(t, 1)
	if r := d.do("toggle", row.Context); r.Message != "Added to Sync · 1 song" {
		t.Fatalf("hold: %+v", r)
	}
}
