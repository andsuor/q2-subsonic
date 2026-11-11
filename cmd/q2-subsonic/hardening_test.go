package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// all asks for a list page by page, as the firmware does, and counts its rows.
func (d *tdaemon) all(op string, args ...string) int {
	n := 0
	for cur := ""; ; {
		r := d.do(op, append(args[:len(args):len(args)], cur)...)
		n += len(r.Rows)
		if cur = r.Cursor; cur == "" {
			return n
		}
	}
}

// A list the firmware asks for 50 rows at a time is fetched from the server once, and a song
// pressed on a page just opened starts without fetching its list again. Choosing asks the server.
func TestBrowseFetchedOnce(t *testing.T) {
	n := &nav{albums: 2, songsPer: 120, artists: 120}
	d := newTestDaemon(t, n, "pages", true)
	d.browseTTL = time.Minute
	for _, c := range []struct {
		op   []string
		rows int
		req  string
	}{
		{[]string{"artists"}, 121, "getArtists"},
		{[]string{"search", "Album"}, 121, "search3"},
		{[]string{"playlist", "playlist:pl1"}, 122, "getPlaylist"}, // Add to Sync, Play, the songs
		{[]string{"playlists"}, 1, "getPlaylists"},
	} {
		if got := d.all(c.op[0], c.op[1:]...); got != c.rows {
			t.Errorf("%v: %d rows, want %d", c.op, got, c.rows)
		}
		d.all(c.op[0], c.op[1:]...)
		if got := n.reqs(c.req); got != 1 {
			t.Errorf("%v: %d %s requests, want 1", c.op, got, c.req)
		}
	}
	d.do("album", "album:al0")
	if r := d.do("play", "song:al0.3 album:al0 3"); r.Message != "Playing Song 3" {
		t.Fatalf("play: %+v", r)
	}
	if got := n.reqs("getAlbum"); got != 1 {
		t.Errorf("album then play: %d getAlbum requests, want 1", got)
	}
	if r := d.do("toggle", "album:al0"); r.Message != "Added to Sync · 120 songs" {
		t.Fatalf("toggle: %+v", r)
	}
	if got := n.reqs("getAlbum"); got != 2 {
		t.Errorf("toggle: %d getAlbum requests, want 2 (it asks the server as it is)", got)
	}
}

// What the cache keeps: nothing without a TTL, no failure, nothing for another account, four lists
// at most, and nothing past its minute or a sign-out.
func TestBrowseCacheBounds(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	cl := d.client()
	calls := 0
	get := func(cl *subsonic.Client, key string, err error) int {
		v, _ := cached(d.daemon, cl, key, func() (int, error) { calls++; return calls, err })
		return v
	}
	if get(cl, "a", nil) == get(cl, "a", nil) {
		t.Fatal("served from the cache with no TTL")
	}
	d.browseTTL = time.Minute
	get(cl, "bad", errors.New("no"))
	if before := calls; get(cl, "bad", nil) != before+1 {
		t.Fatal("a failed fetch was kept")
	}
	if v := get(cl, "a", nil); get(cl, "a", nil) != v {
		t.Fatal("not served from the cache")
	}
	if v := get(cl, "a", nil); get(&subsonic.Client{}, "a", nil) == v {
		t.Fatal("another client was served this one's list")
	}
	for _, k := range []string{"b", "c", "d", "e", "f"} {
		get(cl, k, nil)
	}
	if len(d.cache) != cacheMax {
		t.Fatalf("%d entries, want %d", len(d.cache), cacheMax)
	}
	d.browseTTL = time.Nanosecond
	time.Sleep(time.Millisecond)
	if d.tick(); len(d.cache) != 0 {
		t.Fatalf("tick left %d expired entries", len(d.cache))
	}
	d.browseTTL = time.Minute
	get(cl, "a", nil)
	if d.signOut(); len(d.cache) != 0 {
		t.Fatalf("sign-out left %d entries", len(d.cache))
	}
}

// A panic while answering one request is that request's failure, logged with its trace; the app
// answers the next.
func TestPanicAnswered(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	run := d.runner
	d.runner = nil // the menu's Sync row asks it
	r := d.do("menu")
	d.runner = run
	if r.Status != q2spot.Error || r.Message != "Something went wrong" {
		t.Fatalf("%+v", r)
	}
	if r = d.do("menu"); r.Status != "" || len(r.Rows) == 0 {
		t.Fatalf("the next request: %+v", r)
	}
	b, _ := os.ReadFile(filepath.Join(d.dir, "log.txt"))
	if !strings.Contains(string(b), "panic in menu") || !strings.Contains(string(b), "syncSubtitle") {
		t.Fatalf("log:\n%s", b)
	}
}

// A panic that left d.mu held would hang every later request: the app exits 2, as an unrecovered
// panic does, and the launcher starts it again.
func TestPanicUnderLockExits(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	code, oldExit, oldWait := 0, exit, lockWait
	exit, lockWait = func(c int) { code = c }, 50*time.Millisecond
	t.Cleanup(func() { exit, lockWait = oldExit, oldWait })
	res := d.resolved
	d.resolved = nil // the album page writes it under d.mu
	r := d.do("album", "album:al0")
	if code != 2 {
		t.Fatalf("exit code %d, want 2 (%+v)", code, r)
	}
	d.resolved = res
	d.mu.Unlock()
}

// A setting that can't be written says so, and the row keeps the old quality.
func TestSettingsSaveFails(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	os.Mkdir(filepath.Join(d.dir, "settings.json"), 0o755)
	for _, op := range []string{"defq", "streamq"} {
		if r := d.do(op); r.Status != q2spot.Error || r.Message != "Couldn't save the setting" {
			t.Fatalf("%s: %+v", op, r)
		}
	}
	if _, ok := find(d.do("settings"), "Default Quality: Original"); !ok {
		t.Fatalf("%q", titles(d.do("settings")))
	}
}

// Without the firmware's "pages" word a long list keeps its More… row: no message with the cursor.
func TestNoPagesWithoutCap(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 120, songsPer: 1}, "playlist library", true)
	if r := d.do("albums"); r.Cursor == "" || r.Message != "" {
		t.Fatalf("cursor %q, message %q", r.Cursor, r.Message)
	}
}
