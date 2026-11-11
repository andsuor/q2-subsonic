package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/creds"
	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/plan"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/selection"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
	"github.com/andsuor/q2-subsonic/internal/syncer"
)

// nav is a Navidrome stand-in: albums al0..alN-1 ("Album 000"…) by "Artist", each with songsPer
// songs al<i>.<j>; playlist pl1 "Mix" holds al0's songs; artist ar1 has al0 and al1.
type nav struct {
	albumArtist      string // the albums' own artist, when not "Artist"
	albums, songsPer int
	stall            bool                                                    // every request hangs until the client gives up
	errCode          int                                                     // every API call fails with this Subsonic code
	block            chan struct{}                                           // downloads wait for it
	failID           string                                                  // getAlbum fails for this album only
	failCode         int                                                     // the Subsonic code failID fails with; 0 means 70, "Album not found"
	albumReqs        atomic.Int32                                            // getAlbum calls served
	playlists        int                                                     // playlists pl1..plN ("Mix", "Mix 2"…); 0 means 1. pl<k> holds al<k-1>'s songs
	empty            bool                                                    // every list reply is empty
	lastRange        atomic.Value                                            // the Range header of the latest download, a string
	various          bool                                                    // albums are compilations: album artist "Various Artists", song j by "Artist j"
	mediaErr         int                                                     // download and stream answer this Subsonic error code
	dup              bool                                                    // each playlist holds its first song twice, last too
	artists          int                                                     // getArtists lists this many more artists, ar2.., with no albums
	groups           int                                                     // getArtists spreads its artists over this many index groups, named A, B…
	listErr          int                                                     // getAlbumList2 fails with this code from offset 50 on
	sameAlbums       bool                                                    // getAlbumList2 ignores offset: every page is the first
	holdAlbum        chan struct{}                                           // getAlbum waits for it
	albumGaveUp      atomic.Int32                                            // getAlbum requests the client cancelled while waiting
	paged            bool                                                    // search3 answers the empty query ("") with every song, newest first, in pages
	pageSame         bool                                                    // the empty query ignores songOffset: every page is the first
	pageEmptyAt      int                                                     // the page at this offset is ok and empty, as Navidrome answers a failed search
	pageErr          int                                                     // the empty query fails with this Subsonic code
	songReqs         atomic.Int32                                            // empty-query search3 calls served
	holdSongs        chan struct{}                                           // the empty query's later pages wait for it
	holdPlaylists    chan struct{}                                           // getPlaylist waits for it, but for pl1
	lastScan         atomic.Value                                            // getScanStatus's lastScan, a string; unset, the call is not found (not Navidrome)
	count            atomic.Int64                                            // getScanStatus's count
	scanning         atomic.Bool                                             // getScanStatus's scanning
	scanMoves        bool                                                    // lastScan is another at every call
	scanReqs         atomic.Int32                                            // getScanStatus calls served
	noLastfm         bool                                                    // getTopSongs and getSimilarSongs2 are not implemented (HTTP 501)
	simCode          int                                                     // getSimilarSongs2 fails with this Subsonic code
	art              bool                                                    // albums and playlists name a cover: their own id
	cover            func(id string, w http.ResponseWriter, r *http.Request) // getCoverArt's answer; nil is a 64 px JPEG
	noArtistID       bool                                                    // songs name no artistId
	noAlbums         bool                                                    // getArtist lists no albums
	reqMu            sync.Mutex
	reqN             map[string]int        // requests served, by endpoint
	lastQ            map[string]url.Values // the latest request's query, by endpoint
}

func (n *nav) query(op string) url.Values {
	n.reqMu.Lock()
	defer n.reqMu.Unlock()
	return n.lastQ[op]
}

func (n *nav) reqs(op string) int {
	n.reqMu.Lock()
	defer n.reqMu.Unlock()
	return n.reqN[op]
}

func (n *nav) song(al, j int) map[string]any {
	artist := "Artist"
	if n.various {
		artist = fmt.Sprintf("Artist %d", j)
	}
	s := map[string]any{"id": fmt.Sprintf("al%d.%d", al, j), "title": fmt.Sprintf("Song %d", j), "album": fmt.Sprintf("Album %03d", al),
		"albumId": fmt.Sprintf("al%d", al), "artist": artist, "year": 2020, "discNumber": 1, "track": j + 1,
		"duration": 100, "size": 1000, "suffix": "flac"}
	if !n.noArtistID {
		s["artistId"] = "ar1"
	}
	return s
}

func (n *nav) album(i int) map[string]any {
	artist := cmp.Or(n.albumArtist, "Artist")
	if n.various {
		artist = "Various Artists"
	}
	a := map[string]any{"id": fmt.Sprintf("al%d", i), "name": fmt.Sprintf("Album %03d", i), "artist": artist, "year": 2020, "songCount": n.songsPer}
	if n.art {
		a["coverArt"] = a["id"]
	}
	return a
}

func (n *nav) playlist(k int) map[string]any {
	name := "Mix"
	if k > 1 {
		name = fmt.Sprintf("Mix %d", k)
	}
	count := n.songsPer
	if n.dup {
		count++
	}
	p := map[string]any{"id": fmt.Sprintf("pl%d", k), "name": name, "songCount": count}
	if n.art {
		p["coverArt"] = p["id"]
	}
	return p
}

func (n *nav) songs(al int) []map[string]any {
	var out []map[string]any
	for j := 0; j < n.songsPer; j++ {
		out = append(out, n.song(al, j))
	}
	return out
}

func (n *nav) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if n.stall {
		<-r.Context().Done()
		return
	}
	q := r.URL.Query()
	ok := func(k string, v any) {
		m := map[string]any{"status": "ok", "version": "1.16.1"}
		if k != "" {
			m[k] = v
		}
		json.NewEncoder(w).Encode(map[string]any{"subsonic-response": m})
	}
	op := path.Base(r.URL.Path)
	n.reqMu.Lock()
	if n.reqN == nil {
		n.reqN, n.lastQ = map[string]int{}, map[string]url.Values{}
	}
	n.reqN[op]++
	n.lastQ[op] = q
	n.reqMu.Unlock()
	if n.errCode != 0 && op != "download" && op != "stream" {
		json.NewEncoder(w).Encode(map[string]any{"subsonic-response": map[string]any{"status": "failed",
			"error": map[string]any{"code": n.errCode, "message": "Data not found"}}})
		return
	}
	albumOf := func(id string) int { i, _ := strconv.Atoi(strings.TrimPrefix(id, "al")); return i }
	switch op {
	case "ping", "scrobble":
		ok("", nil)
	case "getAlbumList2":
		if off, _ := strconv.Atoi(q.Get("offset")); n.listErr != 0 && off >= 50 {
			json.NewEncoder(w).Encode(map[string]any{"subsonic-response": map[string]any{"status": "failed",
				"error": map[string]any{"code": n.listErr, "message": "Data not found"}}})
			return
		}
		if n.empty {
			ok("albumList2", map[string]any{})
			return
		}
		off, _ := strconv.Atoi(q.Get("offset"))
		size, _ := strconv.Atoi(q.Get("size"))
		if n.sameAlbums {
			off = 0
		}
		var al []map[string]any
		for i := off; i < n.albums && i < off+size; i++ {
			al = append(al, n.album(i))
		}
		ok("albumList2", map[string]any{"album": al})
	case "getAlbum":
		n.albumReqs.Add(1)
		if n.holdAlbum != nil {
			select {
			case <-n.holdAlbum:
			case <-r.Context().Done():
				n.albumGaveUp.Add(1)
				return
			}
		}
		if q.Get("id") == n.failID {
			code, msg := 70, "Album not found"
			if n.failCode != 0 {
				code, msg = n.failCode, "Server busy"
			}
			json.NewEncoder(w).Encode(map[string]any{"subsonic-response": map[string]any{"status": "failed",
				"error": map[string]any{"code": code, "message": msg}}})
			return
		}
		a := n.album(albumOf(q.Get("id")))
		a["song"] = n.songs(albumOf(q.Get("id")))
		ok("album", a)
	case "getPlaylists":
		var pls []any
		for k := 1; k <= max(n.playlists, 1) && !n.empty; k++ {
			pls = append(pls, n.playlist(k))
		}
		ok("playlists", map[string]any{"playlist": pls})
	case "getPlaylist":
		kk, _ := strconv.Atoi(strings.TrimPrefix(q.Get("id"), "pl"))
		if n.holdPlaylists != nil && kk > 1 {
			select {
			case <-n.holdPlaylists:
			case <-r.Context().Done():
				return
			}
		}
		p := n.playlist(kk)
		p["entry"] = n.songs(kk - 1)
		if n.dup {
			p["entry"] = append(n.songs(kk-1), n.song(kk-1, 0))
		}
		ok("playlist", p)
	case "getArtists":
		if n.empty {
			ok("artists", map[string]any{})
			return
		}
		ars := []any{map[string]any{"id": "ar1", "name": "Artist", "albumCount": 2}}
		for i := 0; i < n.artists; i++ {
			ars = append(ars, map[string]any{"id": fmt.Sprintf("ar%d", i+2), "name": fmt.Sprintf("Artist %05d", i+2), "albumCount": 1})
		}
		ix := []any{map[string]any{"artist": ars}}
		if n.groups > 0 {
			ix = nil
			for g := 0; g < n.groups; g++ {
				var in []any
				for i := g; i < len(ars); i += n.groups {
					in = append(in, ars[i])
				}
				ix = append(ix, map[string]any{"name": string(rune('A' + g)), "artist": in})
			}
		}
		ok("artists", map[string]any{"index": ix})
	case "getArtist":
		if n.noAlbums {
			ok("artist", map[string]any{"id": "ar1", "name": "Artist"})
			return
		}
		ok("artist", map[string]any{"id": "ar1", "name": "Artist", "albumCount": 2, "album": []any{n.album(0), n.album(1)}})
	case "getScanStatus":
		ls, _ := n.lastScan.Load().(string)
		if ls == "" {
			http.NotFound(w, r)
			return
		}
		if k := n.scanReqs.Add(1); n.scanMoves {
			ls += strconv.Itoa(int(k))
		}
		ok("scanStatus", map[string]any{"scanning": n.scanning.Load(), "count": n.count.Load(), "lastScan": ls})
	case "search3":
		if q.Get("query") == `""` {
			n.songReqs.Add(1)
			off, _ := strconv.Atoi(q.Get("songOffset"))
			size, _ := strconv.Atoi(q.Get("songCount"))
			if n.holdSongs != nil && off > 0 {
				select {
				case <-n.holdSongs:
				case <-r.Context().Done():
					return
				}
			}
			switch {
			case n.pageErr != 0:
				json.NewEncoder(w).Encode(map[string]any{"subsonic-response": map[string]any{"status": "failed",
					"error": map[string]any{"code": n.pageErr, "message": "Server busy"}}})
				return
			case !n.paged || n.pageEmptyAt > 0 && off == n.pageEmptyAt:
				ok("searchResult3", map[string]any{})
				return
			case n.pageSame:
				off = 0
			}
			var so []map[string]any // not in album order, as Navidrome's are in the order they were added
			for k := off; k < n.albums*n.songsPer && k < off+size; k++ {
				so = append(so, n.song(n.albums-1-k/n.songsPer, n.songsPer-1-k%n.songsPer))
			}
			ok("searchResult3", map[string]any{"song": so})
			return
		}
		if n.empty {
			ok("searchResult3", map[string]any{})
			return
		}
		ok("searchResult3", map[string]any{"album": []any{n.album(0)}, "song": n.songs(0)})
	case "getStarred2":
		if n.empty {
			ok("starred2", map[string]any{})
			return
		}
		ok("starred2", map[string]any{"artist": []any{map[string]any{"id": "ar1", "name": "Artist", "albumCount": 2}},
			"album": []any{n.album(0)}, "song": n.songs(1)})
	case "getGenres":
		if n.empty {
			ok("genres", map[string]any{})
			return
		}
		ok("genres", map[string]any{"genre": []any{map[string]any{"value": "Rock", "albumCount": 2, "songCount": 20},
			map[string]any{"value": "", "albumCount": 1}, map[string]any{"value": "ambient"},
			map[string]any{"value": "Hip Hop: Old", "albumCount": 1}}})
	case "getRandomSongs", "getTopSongs", "getSimilarSongs2":
		key := map[string]string{"getRandomSongs": "randomSongs", "getTopSongs": "topSongs", "getSimilarSongs2": "similarSongs2"}[op]
		switch {
		case n.noLastfm && op != "getRandomSongs":
			http.Error(w, "This endpoint is not implemented", http.StatusNotImplemented)
		case n.simCode != 0 && op == "getSimilarSongs2":
			json.NewEncoder(w).Encode(map[string]any{"subsonic-response": map[string]any{"status": "failed",
				"error": map[string]any{"code": n.simCode, "message": "data not found"}}})
		case n.empty:
			ok(key, map[string]any{})
		default:
			ok(key, map[string]any{"song": n.songs(0)})
		}
	case "getSong":
		al, j, _ := strings.Cut(strings.TrimPrefix(q.Get("id"), "al"), ".")
		a, _ := strconv.Atoi(al)
		k, _ := strconv.Atoi(j)
		ok("song", n.song(a, k))
	case "getCoverArt":
		if n.cover != nil {
			n.cover(q.Get("id"), w, r)
			return
		}
		w.Write(picture("jpeg", 64, 64))
	case "download", "stream":
		if n.mediaErr != 0 {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"subsonic-response": map[string]any{"status": "failed",
				"error": map[string]any{"code": n.mediaErr, "message": "Song not found"}}})
			return
		}
		if n.block != nil {
			select {
			case <-n.block:
			case <-r.Context().Done():
				return
			}
		}
		if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") && strings.HasSuffix(rg, "-") {
			from, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
			n.lastRange.Store(rg)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-999/1000", from))
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte(strings.Repeat("x", 1000-from)))
			return
		}
		w.Write([]byte(strings.Repeat("x", 1000)))
	default:
		http.NotFound(w, r)
	}
}

type tdaemon struct {
	*daemon
	tmp string
}

func newTestDaemon(t *testing.T, n *nav, caps string, signedIn bool) *tdaemon {
	t.Helper()
	return newTestDaemonOn(t, t.TempDir(), n, caps, signedIn)
}

// newTestDaemonOn starts the daemon on an existing card folder.
func newTestDaemonOn(t *testing.T, card string, n *nav, caps string, signedIn bool) *tdaemon {
	t.Helper()
	tmp := t.TempDir()
	if caps != "" {
		os.WriteFile(filepath.Join(tmp, "q2-addon.caps"), []byte(caps+"\n"), 0o644)
	}
	srv := httptest.NewTLSServer(n)
	d := newDaemon(filepath.Join(card, ".subsonic"), card, tmp)
	d.newClient = func(c subsonic.Creds) (*subsonic.Client, error) { return subsonic.New(c, srv.Client()) }
	d.signinPort = 0
	d.browseTTL = 0 // tests see the server as it is; the cache's own tests set it
	d.runner.CardMounted = func() bool { return true }
	d.free = func(string) int64 { return 1 << 40 }
	if signedIn {
		if err := creds.Save(d.dir, subsonic.Creds{Server: srv.URL, User: "u", Password: "pw"}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		d.signin.stop()
		d.stream.stop()
		d.runner.Cancel()
		srv.CloseClientConnections()
		srv.Close()
		d.bg.Wait() // a ping or preview still writing log.txt would race the TempDir's removal (M45)
		d.stream.stop()
		d.runner.Cancel()
	})
	return &tdaemon{d, tmp}
}

func (d *tdaemon) do(op string, args ...string) q2spot.Reply {
	return d.device(q2spot.Request{ID: "1", Op: op, Args: args})
}

func titles(r q2spot.Reply) []string {
	var out []string
	for _, row := range r.Rows {
		out = append(out, row.Title)
	}
	return out
}

func find(r q2spot.Reply, title string) (q2spot.Row, bool) {
	for _, row := range r.Rows {
		if row.Title == title || strings.HasPrefix(row.Title, title) {
			return row, true
		}
	}
	return q2spot.Row{}, false
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Signed out, the root offers Sign In from Your Phone; the sign-in page (a port on the LAN, and
// hold=1) starts only when it is pressed, then the row opens the QR page (M4).
func TestRootSignedOut(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", false)
	r := d.do("menu")
	if len(r.Rows) != 1 || r.Rows[0].Kind != "action" || r.Rows[0].URI != "signin" || r.Rows[0].Title != "Sign In from Your Phone" {
		t.Fatalf("rows %+v", r.Rows)
	}
	if d.signin.active() {
		t.Fatal("sign-in started without a press")
	}
	if r := d.do("signin"); r.Message != "Press again for the QR code" {
		t.Errorf("toast %q: no code is on screen until the next press", r.Message)
	}
	if r := d.do("menu"); r.Rows[0].Kind != "remote" || !d.signin.active() {
		t.Fatalf("after the press %+v", r.Rows)
	}
}

// Leaving the QR page ends the sign-in, as the spec has it: nothing listens once the user has gone
// (M4).
func TestLeavingQRPageStopsSignIn(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", false)
	d.do("signin")
	d.do("menu")
	d.mu.Lock()
	d.lastMenu = time.Now().Add(-2 * qrLeftAfter) // the QR page was open: no refresh
	d.mu.Unlock()
	if r := d.do("menu"); r.Rows[0].Kind != "action" || d.signin.active() {
		t.Fatalf("sign-in still on: %+v", r.Rows)
	}
}

func TestRootSignedIn(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	r := d.do("menu")
	want := []string{"Sync Entire Library", "Sync", "On This Q2", "Playlists", "Albums", "Artists", "Discover", "Search", "Settings"}
	if got := titles(r); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("titles %q, want %q", got, want)
	}
	kinds := []string{"action", "list", "list", "list", "list", "list", "list", "search", "list"}
	for i, row := range r.Rows {
		if row.Kind != kinds[i] {
			t.Errorf("%s kind %q, want %q", row.Title, row.Kind, kinds[i])
		}
	}
	if !strings.HasPrefix(r.Rows[2].Subtitle, "0 items · ") {
		t.Errorf("On This Q2 subtitle %q", r.Rows[2].Subtitle)
	}
}

func TestHoldToggleToast(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 14}, "", true)
	row, _ := find(d.do("albums"), "Album 000")
	if row.Position != "toggle" || row.Context != "album:al0" {
		t.Fatalf("hold fields %+v", row)
	}
	if r := d.do(row.Position, row.Context); r.Message != "Added to Sync · 14 songs" {
		t.Fatalf("toast %q", r.Message)
	}
	if r := d.do(row.Position, row.Context); r.Message != "Removed from Sync" {
		t.Fatalf("toast %q", r.Message)
	}
}

func TestIncludedVia(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 4}, "", true)
	d.do("toggle", "album:al0")
	page := d.do("album", "album:al0")
	song, ok := find(page, "Song 1")
	if !ok || song.Position != "toggle" {
		t.Fatalf("song row %+v in %q", song, titles(page))
	}
	if r := d.do("toggle", song.Context); r.Message != "Included via Album 000" {
		t.Fatalf("toast %q", r.Message)
	}
	sel, _ := selection.Load(filepath.Join(d.dir, "selection.json"))
	if len(sel.Items) != 1 || sel.Items[0].ID != "al0" {
		t.Fatalf("selection changed: %+v", sel.Items)
	}
}

func TestStatusSubtitles(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 4, songsPer: 3}, "", true)
	sel := selection.Set{Version: 1}
	for _, id := range []string{"al0", "al1", "al2"} {
		sel.Toggle(selection.Item{Kind: selection.KindAlbum, ID: id, Name: id, Quality: "original"})
	}
	sel.Save(filepath.Join(d.dir, "selection.json"))
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	add := func(id, key string) {
		m.Songs[id] = &library.Entry{ID: id, Path: filepath.Join(d.card, "Subsonic", id+".flac"), Size: 1000, Items: []string{key},
			Quality: "original"}
	}
	for j := 0; j < 3; j++ {
		add(fmt.Sprintf("al0.%d", j), "album:al0")
	}
	add("al2.0", "album:al2")
	if err := m.Save(d.card, filepath.Join(d.card, ".subsonic", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	r := d.do("albums")
	want := map[string]string{"Album 000": "Artist · On Q2", "Album 001": "Artist · Waiting to sync",
		"Album 002": "Artist · 1 of 3 songs on Q2", "Album 003": "Artist"}
	for title, sub := range want {
		if row, _ := find(r, title); row.Subtitle != sub {
			t.Errorf("%s subtitle %q, want %q", title, row.Subtitle, sub)
		}
	}
}

func TestPagingMore(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 120, songsPer: 1}, "pages", true)
	r := d.do("albums")
	if len(r.Rows) != 50 || r.Cursor == "" {
		t.Fatalf("first page: %d rows, cursor %q", len(r.Rows), r.Cursor)
	}
	r = d.do("albums", r.Cursor)
	if len(r.Rows) != 50 || r.Rows[0].Title != "Album 050" || r.Cursor == "" {
		t.Fatalf("second page: %d rows from %q, cursor %q", len(r.Rows), r.Rows[0].Title, r.Cursor)
	}
	if r.Message != "pages" {
		t.Errorf("paged reply Message %q, want pages (F-R8: the firmware auto-pages it)", r.Message)
	}
	r = d.do("albums", r.Cursor)
	if len(r.Rows) != 20 || r.Cursor != "" || r.Message != "" {
		t.Fatalf("last page: %d rows, cursor %q, message %q", len(r.Rows), r.Cursor, r.Message)
	}
	for _, op := range [][]string{{"artists"}, {"search", "Album"}, {"artist", "ar0"}} {
		if r := d.do(op[0], op[1:]...); r.Cursor != "" && r.Message != "pages" {
			t.Errorf("%v: cursor %q with Message %q", op, r.Cursor, r.Message)
		}
	}
}

// preview asks for sync until the preview is ready.
func (d *tdaemon) preview(t *testing.T) q2spot.Reply {
	t.Helper()
	var r q2spot.Reply
	eventually(t, "the preview", func() bool { r = d.do("sync"); return r.Message != "refresh" })
	return r
}

func TestSyncPreviewRefreshThenRows(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 2}, "", true)
	d.do("toggle", "album:al0")
	r := d.do("sync")
	if r.Message != "refresh" || len(r.Rows) == 0 || r.Rows[0].Title != "Checking server…" {
		t.Fatalf("first reply %q %q", r.Message, titles(r))
	}
	r = d.preview(t)
	for _, w := range []string{"+2 songs · ", "Needs ", "Start Sync"} {
		if _, ok := find(r, w); !ok {
			t.Errorf("no %q row in %q", w, titles(r))
		}
	}
	if row, _ := find(r, "Start Sync"); row.Kind != "action" || row.URI != "start" {
		t.Errorf("Start Sync row %+v", row)
	}
}

func TestSyncNotEnoughSpace(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 2}, "", true)
	d.free = func(string) int64 { return 0 }
	d.do("toggle", "album:al0")
	r := d.preview(t)
	row, ok := find(r, "Not enough space · Free ")
	if !ok || row.Kind != "info" {
		t.Fatalf("no info Not enough space row in %q", titles(r))
	}
	if _, ok := find(r, "Start Sync"); ok {
		t.Error("Start Sync offered without space")
	}
}

func TestProgressRowKindByCaps(t *testing.T) {
	snap := syncer.Snapshot{State: "running", Permille: 470, Done: 112, Total: 240, ETA: 6 * time.Minute, Current: "Artist – Song"}
	d := newTestDaemon(t, &nav{}, "progress", true)
	r := d.syncRows(snap)
	if r.Message != "refresh" || r.Rows[0].Kind != "progress" || r.Rows[0].Position != "470" {
		t.Fatalf("with progress: %q %+v", r.Message, r.Rows[0])
	}
	for _, w := range []string{"112 of 240 songs", "About 6 min left", "Now: Artist – Song", "Pause", "Cancel"} {
		if _, ok := find(r, w); !ok {
			t.Errorf("no %q in %q", w, titles(r))
		}
	}
	d = newTestDaemon(t, &nav{}, "", true)
	r = d.syncRows(snap)
	if r.Rows[0].Kind != "info" || !strings.HasPrefix(r.Rows[0].Title, "[") || r.Rows[0].Title != "[#####-----] 47 %" {
		t.Fatalf("without progress: %+v", r.Rows[0])
	}
}

func TestDetailsRow(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1, errCode: 70}, "", true)
	r := d.do("album", "album:al0")
	det, ok := find(r, "Details")
	if !ok || det.Kind != "list" || det.URI != "details" {
		t.Fatalf("no Details row in %+v", r.Rows)
	}
	if r.Rows[0].Title != "Data not found" {
		t.Errorf("message row %q", r.Rows[0].Title)
	}
	got := strings.Join(titles(d.do(det.URI, det.Context)), "|")
	for _, w := range []string{"Op: getAlbum", "HTTP: 200", "Code: 70", "Time: "} {
		if !strings.Contains(got, w) {
			t.Errorf("details %q lack %q", got, w)
		}
	}
}

func TestReplyWithin8s(t *testing.T) {
	d := newTestDaemon(t, &nav{stall: true}, "library sections", true)
	ops := [][]string{{"menu"}, {"menu", "home"}, {"menu", "albums"}, {"playlists"}, {"albums"}, {"artists"}, {"artist", "artist:ar1"}, {"search", "x"},
		{"playlist", "playlist:pl1"}, {"album", "album:al0"}, {"toggle", "album:al0"}, {"toggle", "song:al0.1 album:al0 1"},
		{"play", "album:al0 0"}, {"onq2"}, {"settings"}, {"account"}, {"sync"}, {"library"}, {"details", "getAlbum|0|0|0|"},
		{"discover"}, {"albumlist", "newest"}, {"albumlist", "random"}, {"starred"}, {"genres"}, {"genre", "Rock"}, {"shuffle", ""},
		{"shuffle", "artist:ar1"}, {"play", "artist:ar1 0"}, {"radio", "artist:ar1"}, {"top", "artist:ar1"}}
	var wg sync.WaitGroup
	for _, op := range ops {
		wg.Add(1)
		go func(op []string) {
			defer wg.Done()
			start := time.Now()
			d.do(op[0], op[1:]...)
			if took := time.Since(start); took >= 8*time.Second {
				t.Errorf("%v took %v", op, took)
			}
		}(op)
	}
	wg.Wait()
}

func TestStateHoldDuringSync(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2, block: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	state := func() string {
		d.tick()
		b, _ := os.ReadFile(filepath.Join(d.tmp, "q2-subsonic.state"))
		return string(b)
	}
	d.do("toggle", "album:al0")
	d.preview(t)
	if r := d.do("start"); r.Status != "" && r.Status != q2spot.OK {
		t.Fatalf("start: %+v", r)
	}
	eventually(t, "the sync to run", d.runner.Busy)
	if st := state(); !strings.Contains(st, "hold=1") || !strings.Contains(st, "version="+version) {
		t.Fatalf("state during sync:\n%s", st)
	}
	if r := d.do("menu"); !strings.HasPrefix(r.Rows[1].Subtitle, "Syncing · ") {
		t.Errorf("Sync subtitle while running %q", r.Rows[1].Subtitle)
	}
	close(n.block)
	eventually(t, "the sync to finish", func() bool { return d.runner.Snapshot().State == "done" })
	if st := state(); strings.Contains(st, "hold=1") {
		t.Fatalf("hold kept after the sync:\n%s", st)
	}
	if r := d.do("sync"); r.Rows[0].Title != "Sync complete · 2 songs · 0 failed" {
		t.Errorf("done rows %q", titles(r))
	}
}

// A fast failure while the preview is still starting its workers (run with -race).
func TestPreviewFailsFastWithoutRace(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 40, songsPer: 1, failID: "al0", failCode: 10}, "", true)
	sel := selection.Set{Version: 1}
	for i := 0; i < 40; i++ {
		sel.Toggle(selection.Item{Kind: selection.KindAlbum, ID: fmt.Sprintf("al%d", i), Name: "x", Quality: "original"})
	}
	sel.Save(filepath.Join(d.dir, "selection.json"))
	r := d.preview(t)
	if r.Rows[0].Title != "Server busy" {
		t.Fatalf("preview rows %q", titles(r))
	}
	if _, ok := find(r, "Start Sync"); ok {
		t.Fatal("a failed preview offers Start Sync")
	}
}

// A chosen album the server no longer has (code 70, after a library reorganisation) no longer fails
// the whole sync: the rest syncs, the album reads as not on the server, and its songs stay on the
// card until it is taken out of the selection.
func TestPreviewGoneItemKeepsItsSongs(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 2}, "", true)
	d.do("toggle", "album:al0")
	d.do("toggle", "album:al1")
	d.preview(t)
	d.do("start")
	eventually(t, "the first sync to finish", func() bool { return d.runner.Snapshot().State == "done" })

	d2 := newTestDaemonOn(t, filepath.Dir(d.dir), &nav{albums: 3, songsPer: 2, failID: "al0"}, "", true)
	d2.do("toggle", "album:al2")
	r := d2.preview(t)
	if _, ok := find(r, "1 item no longer on the server"); !ok {
		t.Errorf("no gone row: %q", titles(r))
	}
	if _, ok := find(r, "Start Sync"); !ok {
		t.Fatalf("a gone album failed the preview: %q", titles(r))
	}
	if _, ok := find(r, "−"); ok {
		t.Fatalf("the gone album's songs are planned for removal: %q", titles(r))
	}
	q := d2.do("onq2")
	row, ok := find(q, "Album 000")
	if !ok || !strings.Contains(row.Subtitle, "Not on the server") {
		t.Fatalf("gone album row %+v in %q", row, titles(q))
	}
	if r, _ := d2.syncNowRow(); strings.Contains(r.Subtitle, "2 waiting") {
		t.Errorf("the gone album counts as waiting: %+v", r)
	}

	d2.do("start")
	eventually(t, "the second sync to finish", func() bool { return d2.runner.Snapshot().State == "done" })
	if n := len(d2.manifest().Songs); n != 6 {
		t.Fatalf("%d songs on the card, want al0's 2 kept + al1 + al2", n)
	}

	d2.do("toggle", "album:al0") // taken out by hand: now its songs go
	r = d2.preview(t)
	if _, ok := find(r, "−2 songs"); !ok {
		t.Fatalf("taking the gone album out doesn't remove its songs: %q", titles(r))
	}
}

func TestSigninTimeoutNotAutoRestarted(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", false)
	clk := &clock{t: time.Unix(1_000_000, 0)}
	d.signin.now, d.signin.tick = clk.now, 5*time.Millisecond
	d.do("signin")
	if r := d.do("menu"); r.Rows[0].Kind != "remote" || !d.signin.active() {
		t.Fatalf("menu %+v", r.Rows)
	}
	clk.add(11 * time.Minute)
	eventually(t, "the sign-in to time out", func() bool { return !d.signin.active() })
	for i := 0; i < 3; i++ { // the root's 2 s refresh
		r := d.do("menu")
		if row := r.Rows[0]; row.Kind != "action" || row.URI != "signin" || row.Title != "Sign In from Your Phone" {
			t.Fatalf("after timeout %+v", r.Rows)
		}
		if d.signin.active() {
			t.Fatal("the refresh restarted sign-in")
		}
	}
	if r := d.do("signin"); r.Message != "Press again for the QR code" || !d.signin.active() {
		t.Fatalf("signin: %q, active %v", r.Message, d.signin.active())
	}
	if r := d.do("menu"); r.Rows[0].Kind != "remote" {
		t.Fatalf("after pressing %+v", r.Rows)
	}
}

// Ops that stop the sync or the stream answer at once even while a download and a stream fetch hang.
func TestControlOpsReplyFast(t *testing.T) {
	n := &nav{albums: 2, songsPer: 3, block: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	t.Cleanup(func() { close(n.block) })
	d.do("toggle", "album:al0")
	d.preview(t)
	timed := func(op string, args ...string) q2spot.Reply {
		t.Helper()
		start := time.Now()
		r := d.do(op, args...)
		if took := time.Since(start); took >= 2*time.Second {
			t.Errorf("%s took %v", op, took)
		}
		return r
	}
	timed("start")
	eventually(t, "the sync to run", d.runner.Busy)
	timed("play", "album:al1 0")
	eventually(t, "the stream to start", d.stream.active)
	timed("play", "album:al1 1")
	timed("pause")
	timed("resume")
	timed("cancel")
	timed("signout")
	eventually(t, "the stream to stop", func() bool { return !d.stream.active() })
}

func TestSyncNowRowWhenWaiting(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 2}, "", true)
	d.do("toggle", "album:al0")
	for _, c := range [][]string{{"playlists"}, {"albums"}, {"artists"}, {"search", "x"}, {"album", "album:al1"}} {
		r := d.do(c[0], c[1:]...)
		if len(r.Rows) == 0 || r.Rows[0].Title != "Sync Now" || r.Rows[0].Subtitle != "1 waiting" ||
			r.Rows[0].Kind != "list" || r.Rows[0].URI != "sync" {
			t.Errorf("%v: first row %+v", c, r.Rows)
		}
	}
	if r := d.do("albums"); len(r.Rows) < 2 || r.Rows[1].Title != "Album 000" {
		t.Errorf("order changed: %q", titles(r))
	}
}

func TestNoSyncNowRowWhenUpToDate(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 2}, "", true)
	for _, c := range [][]string{{"albums"}, {"playlists"}, {"menu"}} {
		if _, ok := find(d.do(c[0]), "Sync Now"); ok {
			t.Errorf("%v: Sync Now with nothing selected", c)
		}
	}
	d.do("toggle", "album:al0")
	if _, ok := find(d.do("menu"), "Sync Now"); ok {
		t.Error("Sync Now on the root menu")
	}
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	for j := 0; j < 2; j++ {
		id := fmt.Sprintf("al0.%d", j)
		m.Songs[id] = &library.Entry{ID: id, Path: filepath.Join(d.card, "Subsonic", id+".flac"), Size: 1, Items: []string{"album:al0"},
			Quality: "original"}
	}
	if err := m.Save(d.card, filepath.Join(d.card, ".subsonic", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if _, ok := find(d.do("albums"), "Sync Now"); ok {
		t.Error("Sync Now though everything is on the Q2")
	}
}

func TestSyncNowRowKeepsPagingWhole(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 120, songsPer: 1}, "", true)
	d.do("toggle", "album:al0")
	r := d.do("albums")
	if len(r.Rows) != 50 || r.Rows[0].Title != "Sync Now" || r.Rows[49].Title != "Album 048" || r.Cursor != "49" {
		t.Fatalf("first page %d rows, last %q, cursor %q", len(r.Rows), r.Rows[len(r.Rows)-1].Title, r.Cursor)
	}
	if r = d.do("albums", r.Cursor); r.Rows[0].Title != "Album 049" {
		t.Errorf("second page starts %q", r.Rows[0].Title)
	}
}

func TestLibraryHelpCopy(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	got := strings.Join(titles(d.do("library")), "|")
	for _, w := range []string{"Stop Streaming Services", "Run after the sync has finished and Subsonic is closed",
		"Import playlist only applies to synced playlists"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in %q", w, got)
		}
	}
}

func (d *tdaemon) selected(t *testing.T) []selection.Item {
	t.Helper()
	sel, err := selection.Load(filepath.Join(d.dir, "selection.json"))
	if err != nil {
		t.Fatal(err)
	}
	return sel.Items
}

func TestEntireLibraryRow(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 2, playlists: 2}, "", true)
	r := d.do("menu")
	row := r.Rows[0]
	if row.Title != "Sync Entire Library" || row.Subtitle != "Off" || row.URI != "toggle" ||
		row.Context != "library:all" || row.Position != "toggle" || row.Kind != "action" {
		t.Fatalf("row %+v", row)
	}
	if got := titles(d.do("onq2")); len(got) != 1 || got[0] != "Nothing chosen yet" {
		t.Fatalf("on this q2 while off %q", got)
	}
	if r := d.do(row.URI, row.Context); r.Message != "Added to Sync · Entire Library" {
		t.Fatalf("toast %q", r.Message)
	}
	if it := d.selected(t); len(it) != 1 || it[0].Name != "Entire Library" || it[0].Quality != "original" {
		t.Fatalf("selection %+v", it)
	}
	if got := d.do("menu").Rows[0].Subtitle; got != "On" {
		t.Fatalf("before resolve %q", got)
	}
	d.preview(t)
	if got := d.do("menu").Rows[0].Subtitle; got != "On · 3 albums · 2 playlists" {
		t.Fatalf("after resolve %q", got)
	}
	r = d.do("onq2")
	if len(r.Rows) != 2 || r.Rows[0].Subtitle != "On · 3 albums · 2 playlists" || r.Rows[1].Title != "Quality: Original" {
		t.Fatalf("on this q2 while on %q", titles(r))
	}
	if r := d.do("toggle", "library:all"); r.Message != "Removed from Sync" {
		t.Fatalf("toast %q", r.Message)
	}
	if got := d.do("menu").Rows[0].Subtitle; got != "Off" {
		t.Fatalf("after removal %q", got)
	}
}

func TestEntireLibraryResolvesAlbumsAndPlaylists(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 2, playlists: 2}, "", true)
	d.do("toggle", "library:all")
	if r := d.do("albums"); r.Rows[0].Title != "Sync Now" || r.Rows[0].Subtitle != "1 waiting" {
		t.Fatalf("rows %q %q", titles(r), r.Rows[0].Subtitle)
	}
	d.preview(t)
	p := d.daemon.preview.plan
	if len(p.Add) != 6 {
		t.Errorf("Add %d, want 6", len(p.Add))
	}
	if len(p.Playlists) != 2 || len(p.Playlists["Mix"]) != 2 || len(p.Playlists["Mix 2"]) != 2 {
		t.Errorf("playlists %v", p.Playlists)
	}
}

func TestEntireLibraryIncludedVia(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 2}, "", true)
	d.do("toggle", "library:all")
	sel := selection.Set{Version: 1, Items: d.selected(t)}
	sel.Toggle(selection.Item{Kind: selection.KindAlbum, ID: "al2", Name: "Album 002"})
	sel.Save(filepath.Join(d.dir, "selection.json"))
	for _, key := range []string{"album:al2", "album:al1", "playlist:pl1", "artist:ar1", "song:al0.1"} {
		if r := d.do("toggle", key); r.Message != "Included via Entire Library" {
			t.Errorf("%s: toast %q", key, r.Message)
		}
	}
	if it := d.selected(t); len(it) != 2 || it[0].Kind != selection.KindLibrary || it[1].ID != "al2" {
		t.Fatalf("selection changed: %+v", it)
	}
}

func TestEmptyStates(t *testing.T) {
	d := newTestDaemon(t, &nav{empty: true}, "", true)
	for _, c := range []struct{ op, arg, title, sub string }{
		{"playlists", "", "No playlists", "Playlists must be public on the server to show here"},
		{"albums", "", "No albums", ""},
		{"artists", "", "No artists", ""},
		{"search", "x", "No results", ""},
		{"albumlist", "newest", "No albums", ""},
		{"albumlist", "random", "No albums", ""},
		{"starred", "", "Nothing starred", "Star music on the server to see it here"},
		{"genres", "", "No genres", ""},
		{"genre", "Rock", "No albums", ""},
		{"top", "artist:ar1", "No top songs", ""},
		{"onq2", "", "Nothing chosen yet", "Hold Play/Pause on a playlist, album or artist"},
	} {
		r := d.do(c.op, c.arg)
		row, ok := find(r, c.title)
		if !ok || row.Kind != "info" || row.Subtitle != c.sub {
			t.Errorf("%s: %q row %+v", c.op, titles(r), row)
		}
	}
}

func TestEntireLibraryPagesPast500Albums(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 501, songsPer: 1}, "", true)
	d.do("toggle", "library:all")
	d.preview(t)
	if n := len(d.daemon.preview.plan.Add); n != 501 {
		t.Errorf("Add %d, want 501", n)
	}
}

func TestEntireLibraryFailsFast(t *testing.T) {
	n := &nav{albums: 400, songsPer: 1, failID: "al0"}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "library:all")
	r := d.preview(t)
	if _, ok := find(r, "Start Sync"); ok {
		t.Fatalf("preview should fail: %q", titles(r))
	}
	if got := n.albumReqs.Load(); got > 50 {
		t.Errorf("%d of 400 album requests made after the first failure", got)
	}
}

// syncAlbum runs a one-album sync to done on a daemon with cap library.
func syncAlbum(t *testing.T) *tdaemon {
	t.Helper()
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "library", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to finish", func() bool { return d.runner.Snapshot().State == "done" })
	return d
}

func (d *tdaemon) libReqSeq(t *testing.T) string {
	t.Helper()
	var b []byte
	eventually(t, "the library request", func() bool {
		var err error
		b, err = os.ReadFile(filepath.Join(d.tmp, "q2-subsonic.library"))
		return err == nil
	})
	return strings.SplitN(string(b), "\n", 2)[0]
}

func TestLibraryRequestedAutomaticallyAfterSync(t *testing.T) {
	d := syncAlbum(t)
	seq := d.libReqSeq(t)
	b, _ := os.ReadFile(filepath.Join(d.tmp, "q2-subsonic.library"))
	if !strings.HasPrefix(string(b), seq+"\nscan\n") {
		t.Errorf("request %q", b)
	}
	time.Sleep(100 * time.Millisecond)
	d.do("sync") // opening the page sends nothing more
	if got := d.libReqSeq(t); got != seq {
		t.Errorf("request rewritten: %s -> %s", seq, got)
	}
}

func TestNoLibraryRequestWhenNothingChanged(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "library", true)
	d.syncDone(plan.Plan{})
	if _, err := os.Stat(filepath.Join(d.tmp, "q2-subsonic.library")); err == nil {
		t.Error("a request was written")
	}
	d.syncDone(plan.Plan{PlaylistsStale: true})
	if _, err := os.Stat(filepath.Join(d.tmp, "q2-subsonic.library")); err != nil {
		t.Error("a rewritten playlist sent nothing")
	}
}

func TestLibraryRowShowsPendingThenUpdated(t *testing.T) {
	d := syncAlbum(t)
	seq := d.libReqSeq(t)
	r := d.do("sync")
	if r.Message != "refresh" {
		t.Errorf("pending without refresh: %+v", r)
	}
	if _, ok := find(r, "Updating Library…"); !ok {
		t.Fatalf("pending rows %q", titles(r))
	}
	os.WriteFile(filepath.Join(d.tmp, "q2-subsonic.library.done"), []byte(seq+" ok\n"), 0o644)
	if _, ok := find(d.do("sync"), "Library updated"); !ok {
		t.Errorf("ok rows %q", titles(d.do("sync")))
	}
}

func TestLibraryRowShowsFirmwareError(t *testing.T) {
	d := syncAlbum(t)
	seq := d.libReqSeq(t)
	os.WriteFile(filepath.Join(d.tmp, "q2-subsonic.library.done"), []byte(seq+" error scan busy\n"), 0o644)
	r := d.do("sync")
	if _, ok := find(r, "scan busy"); !ok {
		t.Errorf("error rows %q", titles(r))
	}
	if _, ok := find(r, "Details"); !ok {
		t.Errorf("no Details: %q", titles(r))
	}
}

// A firmware that refuses a playlist name longer than it keeps answers "error Name too long: <name>"
// (issue #1): shown as any firmware error, with the message in Details too.
func TestLibraryRowShowsNameTooLong(t *testing.T) {
	d := syncAlbum(t)
	seq := d.libReqSeq(t)
	msg := "Name too long: " + strings.Repeat("曲", 22)
	os.WriteFile(filepath.Join(d.tmp, "q2-subsonic.library.done"), []byte(seq+" error "+msg+"\n"), 0o644)
	r := d.do("sync")
	if _, ok := find(r, msg); !ok {
		t.Errorf("error rows %q", titles(r))
	}
	det, ok := find(r, "Details")
	if !ok {
		t.Fatalf("no Details: %q", titles(r))
	}
	if _, ok := find(d.do("details", det.Context), "Error: "+msg); !ok {
		t.Errorf("details %q", titles(d.do("details", det.Context)))
	}
}

func TestLibraryRowNoDoubleRequest(t *testing.T) {
	d := syncAlbum(t)
	seq := d.libReqSeq(t)
	d.do("library")
	d.do("library")
	if got := d.libReqSeq(t); got != seq {
		t.Errorf("second request: %s -> %s", seq, got)
	}
}

// A play still waiting for playMu when Sign Out is pressed never starts.
func TestSignOutDropsPendingPlay(t *testing.T) {
	n := &nav{albums: 2, songsPer: 3, block: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	t.Cleanup(func() { close(n.block) })
	d.playMu.Lock() // a previous start still running
	d.do("play", "album:al1 0")
	d.do("signout")
	time.Sleep(50 * time.Millisecond) // let an unguarded stop run first, as in the race
	d.playMu.Unlock()
	time.Sleep(300 * time.Millisecond)
	if d.stream.active() {
		t.Fatal("stream started after sign-out")
	}
}

// A recorded playlist file whose name has an ellipsis is imported: the firmware refuses only a real
// ".." segment (I18).
func TestLibraryRequestKeepsDottedPlaylist(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "library", true)
	good, bad := filepath.Join(d.card, "_explaylist_data", "Mix.m3u"), filepath.Join(d.card, "_explaylist_data", "Wait... What.m3u")
	for _, f := range []string{good, bad} {
		os.MkdirAll(filepath.Dir(f), 0o755)
		os.WriteFile(f, []byte("#EXTM3U\n"), 0o644)
	}
	(&library.Manifest{Playlists: map[string]string{"Mix": good, "Wait... What": bad}}).Save(d.card, filepath.Join(d.card, ".subsonic", "manifest.json"))
	d.syncDone(plan.Plan{PlaylistsStale: true})
	b, err := os.ReadFile(filepath.Join(d.tmp, "q2-subsonic.library"))
	if err != nil || !strings.Contains(string(b), "\nscan\n") || !strings.Contains(string(b), good+"\n") || !strings.Contains(string(b), bad+"\n") {
		t.Fatalf("%q %v", b, err)
	}
}

// A sync paused because the server went away or the card filled says why, with Resume (I3).
func TestPausedSyncSaysWhy(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	d.client()
	for err, want := range map[error]string{syncer.ErrOffline: "Can't reach 127.0.0.1", syncer.ErrCardFull: "The card is full"} {
		r := d.syncRows(syncer.Snapshot{State: "paused", Err: err})
		if _, ok := find(r, want); !ok {
			t.Errorf("%v: %q", err, titles(r))
		}
		if _, ok := find(r, "Resume"); !ok {
			t.Errorf("%v: no Resume: %q", err, titles(r))
		}
	}
}

// The preview shows what a quality change adds or frees (M20).
func TestPreviewReplaceShowsSize(t *testing.T) {
	for want, p := range map[string]*plan.Plan{
		"Replace 2 songs · −5 MB": {Replace: make([]plan.Want, 2), ReplaceBytes: -5e6, Fits: true},
		"Replace 1 song · +3 MB":  {Replace: make([]plan.Want, 1), ReplaceBytes: 3e6, Fits: true},
	} {
		if r := previewRows(p, 0); !strings.Contains(strings.Join(titles(r), "|"), want) {
			t.Errorf("want %q: %q", want, titles(r))
		}
	}
}

// Swapping a synced playlist for the album holding the same songs needs no transfer; the songs'
// items are still recorded, so after a restart the album reads On Q2, not Waiting to sync (I12).
func TestItemsRecordedWithoutTransfer(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "playlist:pl1")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	d.do("toggle", "album:al0")
	d.do("toggle", "playlist:pl1")
	d.do("check")
	d.preview(t)
	d2 := newTestDaemonOn(t, d.card, n, "", true) // a restart: nothing resolved yet
	if st := d2.view().status(selection.KindAlbum, "al0", 2); st != "On Q2" {
		t.Errorf("album after restart: %q", st)
	}
	if _, ok := find(d2.do("albums"), "Sync Now"); ok {
		t.Error("Sync Now after restart")
	}
}

// A sync stopped because the server refused the account (downloads turned off, R39) says why, in
// subsonic.Message's words, the one place that copy lives (review G1, Minor 5).
func TestPausedRefusedSaysWhy(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	d.client()
	err := fmt.Errorf("%w: %w", syncer.ErrRefused, &subsonic.APIError{Op: "download", HTTP: 200, Code: 50, Msg: "User not authorized"})
	want := subsonic.Message(err, d.hostName())
	if r := d.syncRows(syncer.Snapshot{State: "paused", Err: err}); !strings.Contains(strings.Join(titles(r), "|"), "|"+want+"|Resume") {
		t.Errorf("want %q: %q", want, titles(r))
	}
}

// On a server without OpenSubsonic's displayAlbumArtist, a compilation's songs take the album's
// artist, so it syncs as one folder, not one per track artist (M41).
func TestCompilationOneFolder(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 3, various: true}, "", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	d.mu.Lock()
	p := d.daemon.preview.plan
	d.mu.Unlock()
	if len(p.Add) != 3 {
		t.Fatalf("%+v", p)
	}
	for _, w := range p.Add {
		if !strings.HasPrefix(w.Path, filepath.Join(d.card, "Subsonic", "Various Artists")+"/") {
			t.Errorf("%s", w.Path)
		}
	}
}

// syncPlaylist runs a sync of playlist Mix to done on a daemon with cap library.
func syncPlaylist(t *testing.T) *tdaemon {
	t.Helper()
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "library", true)
	d.do("toggle", "playlist:pl1")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to finish", func() bool { return d.runner.Snapshot().State == "done" })
	return d
}

// libReq is the library request on file: its seq and its other lines.
func (d *tdaemon) libReq(t *testing.T) (string, []string) {
	t.Helper()
	d.libReqSeq(t)
	b, _ := os.ReadFile(filepath.Join(d.tmp, "q2-subsonic.library"))
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	return lines[0], lines[1:]
}

func (d *tdaemon) answer(seq string, lines ...string) {
	os.WriteFile(filepath.Join(d.tmp, "q2-subsonic.library.done"), []byte(strings.Join(append([]string{seq}, lines...), "\n")+"\n"), 0o644)
}

// R35: a playlist the app hasn't imported yet is sent as "add", which never replaces a playlist
// the user made; once the firmware confirms it, it is the app's own and later requests replace it.
func TestLibraryAddsThenImports(t *testing.T) {
	d := syncPlaylist(t)
	mix := filepath.Join(d.card, "_explaylist_data", "Mix.m3u")
	seq, lines := d.libReq(t)
	if strings.Join(lines, "|") != "scan|add "+mix {
		t.Fatalf("request %q", lines)
	}
	d.answer(seq + " ok")
	if _, ok := find(d.do("sync"), "Library updated"); !ok {
		t.Fatalf("%q", titles(d.do("sync")))
	}
	if !d.manifest().Imported[mix] {
		t.Fatal("not recorded as imported")
	}
	d.syncDone(plan.Plan{PlaylistsStale: true})
	if seq2, lines := d.libReq(t); seq2 == seq || strings.Join(lines, "|") != "scan|import "+mix {
		t.Fatalf("next request %s %q", seq2, lines)
	}
}

// A playlist file an older version recorded under a name longer than the Q2 keeps is never sent
// (issue #1): the Q2 would add another copy, or refuse the whole request. The next sync moves it.
func TestLibraryNeverSendsLongName(t *testing.T) {
	d := syncPlaylist(t)
	dir := filepath.Join(d.card, "_explaylist_data")
	mix := filepath.Join(dir, "Mix.m3u")
	seq, _ := d.libReq(t)
	d.answer(seq + " ok")
	d.do("sync")
	imp, add := filepath.Join(dir, strings.Repeat("曲", 30)+".m3u"), filepath.Join(dir, strings.Repeat("Песня", 10)+".m3u")
	m := d.manifest()
	m.Playlists["Long 1"], m.Playlists["Long 2"], m.Imported[imp] = imp, add, true
	for _, f := range []string{imp, add} {
		os.WriteFile(f, []byte("#EXTM3U\n"), 0o644)
	}
	if err := m.Save(d.card, filepath.Join(d.card, ".subsonic", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	d.syncDone(plan.Plan{PlaylistsStale: true})
	seq2, lines := d.libReq(t)
	for _, l := range lines {
		if _, f, ok := strings.Cut(l, " "); ok && len(strings.TrimSuffix(filepath.Base(f), ".m3u")) > 63 {
			t.Errorf("sent %q", l)
		}
	}
	if seq2 == seq || strings.Join(lines, "|") != "scan|import "+mix {
		t.Fatalf("request %s %q", seq2, lines)
	}
}

// R35: a playlist of the same name already on the Q2 (the user's own) is never replaced: the file
// is renamed "Mix (Subsonic).m3u" and added again; a second clash is an error with Details.
func TestLibraryExistsRenamesThenClash(t *testing.T) {
	d := syncPlaylist(t)
	dir := filepath.Join(d.card, "_explaylist_data")
	mix, mixS := filepath.Join(dir, "Mix.m3u"), filepath.Join(dir, "Mix (Subsonic).m3u")
	seq, _ := d.libReq(t)
	d.answer(seq+" ok", "exists "+mix)
	if _, ok := find(d.do("sync"), "Updating Library…"); !ok {
		t.Fatalf("%q", titles(d.do("sync")))
	}
	seq2, lines := d.libReq(t)
	if seq2 == seq || strings.Join(lines, "|") != "add "+mixS {
		t.Fatalf("follow-up %s %q", seq2, lines)
	}
	if _, err := os.Stat(mix); !os.IsNotExist(err) {
		t.Error("Mix.m3u kept")
	}
	if m := d.manifest(); m.Playlists["Mix"] != mixS || m.Imported[mix] {
		t.Fatalf("%+v %+v", m.Playlists, m.Imported)
	}
	d.answer(seq2+" ok", "exists "+mixS)
	r := d.do("sync")
	if _, ok := find(r, "Couldn't import Mix (Subsonic)"); !ok {
		t.Fatalf("%q", titles(r))
	}
	if _, ok := find(r, "Details"); !ok {
		t.Fatalf("no Details: %q", titles(r))
	}
}

// One request at a time: one wanted while another awaits its answer is sent once that comes, so
// no answer (and no import to record) is lost.
func TestLibraryOneRequestAtATime(t *testing.T) {
	d := syncPlaylist(t)
	seq, _ := d.libReq(t)
	d.syncDone(plan.Plan{PlaylistsStale: true})
	if seq2, _ := d.libReq(t); seq2 != seq {
		t.Fatal("request replaced before its answer")
	}
	d.answer(seq + " ok")
	d.tick()
	if seq2, lines := d.libReq(t); seq2 == seq || !strings.Contains(strings.Join(lines, "|"), "import ") {
		t.Fatalf("next request %s %q", seq2, lines)
	}
}

// The answer is taken without the Sync page, and by a later run of the app (the request on file
// says what was sent).
func TestLibraryAnswerTakenByTick(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "library", true)
	mix := filepath.Join(d.card, "_explaylist_data", "Mix.m3u")
	os.MkdirAll(filepath.Dir(mix), 0o755)
	os.WriteFile(mix, []byte("#EXTM3U\n"), 0o644)
	(&library.Manifest{Playlists: map[string]string{"Mix": mix}, Imported: map[string]bool{}}).Save(d.card, filepath.Join(d.card, ".subsonic", "manifest.json"))
	os.WriteFile(filepath.Join(d.tmp, "q2-subsonic.library"), []byte("41\nscan\nadd "+mix+"\n"), 0o644)
	d.answer("41 ok")
	d.tick()
	if !d.manifest().Imported[mix] {
		t.Fatal("answer not taken")
	}
}

// M1: Account shows the server without a user:password@ part.
func TestAccountHidesServerPassword(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", false)
	if err := creds.Save(d.dir, subsonic.Creds{Server: "https://alice:proxySecret42@music.example/sub", User: "u", Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	r := d.do("account")
	if len(r.Rows) == 0 || r.Rows[0].Title != "https://music.example/sub" {
		t.Fatalf("rows %+v", r.Rows)
	}
	for _, row := range r.Rows {
		if strings.Contains(row.Title+row.Subtitle, "proxySecret42") {
			t.Fatalf("password on screen: %+v", row)
		}
	}
}

// I23: Account says what is on the card: the password, or only an API key.
func TestAccountSaysKeyOrPassword(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	if _, ok := find(d.do("account"), "Password is stored on this card"); !ok {
		t.Fatal("password sign-in")
	}
	d.signOut()
	creds.Save(d.dir, subsonic.Creds{Server: "https://music.example", User: "u", APIKey: "k"})
	r := d.do("account")
	if _, ok := find(r, "Only an API key is stored on this card"); !ok {
		t.Fatalf("key sign-in: %q", titles(r))
	}
}

// I24: a failed phone sign-in shows on the root after the QR page is left (which restarts sign-in),
// and stays until the next try.
func TestSigninFailureShownOnRoot(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", false)
	d.do("signin")
	d.signin.mu.Lock()
	addr, key := d.signin.ln.Addr().String(), d.signin.key
	d.signin.mu.Unlock()
	req, _ := http.NewRequest("POST", "http://"+addr+"/api/signin",
		strings.NewReader(`{"server":"https://127.0.0.1:1","user":"andreas","password":"x"}`))
	req.Header.Set("X-Q2", "1")
	req.AddCookie(&http.Cookie{Name: signinCookie, Value: key})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	d.mu.Lock()
	d.lastMenu = time.Now().Add(-2 * qrLeftAfter) // the QR page was open: no refresh
	d.mu.Unlock()
	for i := 0; i < 2; i++ {
		r := d.do("menu")
		if r.Rows[0].Kind != "action" || r.Rows[0].Subtitle != "Can't reach 127.0.0.1:1" {
			t.Fatalf("menu %d: %+v", i, r.Rows[0])
		}
	}
}

// A stream that gave up shows below the root's fixed rows, so the row under the cursor stays
// what it was, with Details (M33).
func TestRootStreamErrorLast(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2, mediaErr: 70}, "", true)
	d.stream.backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	d.do("play", "album:al0 0")
	var r q2spot.Reply
	eventually(t, "the stream error", func() bool { r = d.do("menu"); _, ok := find(r, "Couldn't stream Song 0"); return ok })
	if r.Rows[0].Title != "Sync Entire Library" {
		t.Fatalf("first row %q", titles(r))
	}
	det := r.Rows[len(r.Rows)-1]
	if r.Rows[len(r.Rows)-2].Title != "Couldn't stream Song 0" || det.Title != "Details" {
		t.Fatalf("rows %q", titles(r))
	}
	got := strings.Join(titles(d.do(det.URI, det.Context)), "|")
	for _, w := range []string{"Op: stream", "Code: 70", "Song: al0.0"} {
		if !strings.Contains(got, w) {
			t.Errorf("details %q lack %q", got, w)
		}
	}
}

// Songs an earlier run streamed are deleted when the app starts (M26).
func TestStreamFolderEmptiedAtStart(t *testing.T) {
	card := t.TempDir()
	old := filepath.Join(card, ".subsonic", "stream", "0003 - Song [x].flac")
	os.MkdirAll(filepath.Dir(old), 0o755)
	os.WriteFile(old, []byte("x"), 0o644)
	newTestDaemonOn(t, card, &nav{}, "", true)
	if _, err := os.Stat(old); err == nil {
		t.Fatal("left on the card")
	}
}

// But not while the firmware may still play them: the app exits when idle and Go to starts it
// again from Now Playing, with .playing naming a song of that folder.
func TestStreamFolderKeptAtStartWhilePlaying(t *testing.T) {
	card, tmp := t.TempDir(), t.TempDir()
	dir := filepath.Join(card, ".subsonic", "stream")
	os.MkdirAll(dir, 0o755)
	now, next, queue := filepath.Join(dir, "0003 - Song [x].flac"), filepath.Join(dir, "0004 - Song [y].flac"), filepath.Join(dir, "queue-1.m3u")
	for _, f := range []string{now, next, queue, filepath.Join(tmp, "q2-subsonic.urls")} {
		os.WriteFile(f, []byte("x"), 0o644)
	}
	os.WriteFile(filepath.Join(tmp, "q2-subsonic.playing"), []byte(now+"\n"), 0o644)
	newDaemon(filepath.Join(card, ".subsonic"), card, tmp)
	if !exists(now) || !exists(next) || !exists(queue) {
		t.Fatal("the playing stream's files went")
	}
	if exists(filepath.Join(tmp, "q2-subsonic.urls")) {
		t.Error("the URL queue is left in /tmp")
	}
	os.Remove(now) // .playing names a song that is gone: an older stream
	newDaemon(filepath.Join(card, ".subsonic"), card, tmp)
	if exists(next) || exists(queue) {
		t.Fatal("left on the card")
	}
}

// In a playlist a song the server lacks (70, not found) is skipped: the root says so, with Details.
func TestRootStreamSkipped(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1, mediaErr: 70}, "playlist", true)
	d.stream.gap = 0
	d.do("play", "album:al0 0")
	var r q2spot.Reply
	eventually(t, "the skip", func() bool { r = d.do("menu"); _, ok := find(r, "Skipped Song 0: Song not found"); return ok })
	det := r.Rows[len(r.Rows)-1]
	if got := strings.Join(titles(d.do(det.URI, det.Context)), "|"); !strings.Contains(got, "Code: 70") {
		t.Fatalf("details %q", got)
	}
}

// A stream holds the Q2 awake only while it fetches: with the songs around the one playing on the
// card, hold=1 goes, though the stream still runs (I14).
func TestNoHoldWhileStreamWaits(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 6}, "playlist", true)
	d.do("play", "album:al0 0")
	eventually(t, "the window", func() bool { // kept in the stream cache, as the firmware has playlist
		m, _ := filepath.Glob(filepath.Join(d.card, ".subsonic", "stream", "*", "original", "*", "song 2 [[]al0.2].flac"))
		return len(m) == 1
	})
	eventually(t, "hold off", func() bool {
		d.tick()
		b, _ := os.ReadFile(filepath.Join(d.tmp, "q2-subsonic.state"))
		return !strings.Contains(string(b), "hold=1")
	})
	if !d.stream.active() {
		t.Fatal("the stream ended")
	}
}

// The end of a sync counts as activity, and the app stays up while its library request awaits
// the firmware's answer, which the firmware works on only while the app runs (I15).
func TestStaysUpAfterSync(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2, block: make(chan struct{})}
	d := newTestDaemon(t, n, "library", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to run", d.runner.Busy)
	d.mu.Lock()
	d.lastReq = time.Now().Add(-time.Hour) // an unattended sync: no request for an hour
	d.mu.Unlock()
	close(n.block)
	eventually(t, "the sync to finish", func() bool { return d.runner.Snapshot().State == "done" })
	if d.idle(time.Now()) {
		t.Fatal("exits the moment the sync ends")
	}
	seq := d.libReqSeq(t)
	if d.idle(time.Now().Add(2 * idleAfter)) {
		t.Fatal("exits before the library request is answered")
	}
	d.answer(seq + " ok")
	d.tick()
	if !d.idle(time.Now().Add(2 * idleAfter)) {
		t.Fatal("never idles once answered")
	}
}

// More playlists than one request holds (the firmware takes 32 KiB) go in several: scan and the
// first in one, the rest in follow-ups, each sent once the one before is answered (I20).
func TestLibraryRequestSplit(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "library", true)
	m := &library.Manifest{Playlists: map[string]string{}, Imported: map[string]bool{}}
	want := map[string]bool{}
	for i := 0; i < 400; i++ {
		name := fmt.Sprintf("%03d %s", i, strings.Repeat("x", 59)) // 63 bytes, the most the Q2 keeps
		f := filepath.Join(d.card, "_explaylist_data", name+".m3u")
		os.MkdirAll(filepath.Dir(f), 0o755)
		os.WriteFile(f, []byte("#EXTM3U\n"), 0o644)
		m.Playlists[name], m.Imported[f] = f, i%2 == 0
		want[f] = true
	}
	if err := m.Save(d.card, filepath.Join(d.card, ".subsonic", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	d.syncDone(plan.Plan{PlaylistsStale: true})
	sent := map[string]bool{}
	for k := 0; ; k++ {
		b, _ := os.ReadFile(filepath.Join(d.tmp, "q2-subsonic.library"))
		if len(b) >= 32<<10-2 {
			t.Fatalf("request %d: %d bytes, the firmware's limit is 32 KiB", k, len(b))
		}
		seq, lines := d.libReq(t)
		if scan := len(lines) > 0 && lines[0] == "scan"; scan != (k == 0) {
			t.Fatalf("request %d: scan %v", k, scan)
		} else if scan {
			lines = lines[1:]
		}
		for _, l := range lines {
			_, p, _ := strings.Cut(l, " ")
			if sent[p] || !want[p] {
				t.Fatalf("request %d: %q", k, l)
			}
			sent[p] = true
		}
		d.answer(seq + " ok")
		d.tick()
		if d.lib.done {
			break
		}
		if k > 10 {
			t.Fatal("no end")
		}
	}
	if len(sent) != 400 || !d.lib.ok {
		t.Fatalf("%d sent, ok %v", len(sent), d.lib.ok)
	}
}

// syncOK runs a sync of what is chosen to done.
func (d *tdaemon) syncOK(t *testing.T) {
	t.Helper()
	d.do("check")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to finish", func() bool { return d.runner.Snapshot().State == "done" })
}

// A synced playlist that holds a song twice is On Q2, also after a restart (its server count has
// the duplicate), and so is a chosen empty one: no Sync Now for them (I13).
func TestPlaylistDuplicateOnQ2(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3, dup: true}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "playlist:pl1")
	d.syncOK(t)
	for _, dd := range []*tdaemon{d, newTestDaemonOn(t, d.card, n, "", true)} {
		r := dd.do("playlists")
		if row, _ := find(r, "Mix"); row.Subtitle != "4 songs · On Q2" {
			t.Errorf("Mix %q", row.Subtitle)
		}
		if _, ok := find(r, "Sync Now"); ok {
			t.Error("Sync Now")
		}
	}
	e := newTestDaemon(t, &nav{albums: 1, songsPer: 0}, "", true)
	e.do("toggle", "playlist:pl1")
	e.syncOK(t)
	if row, _ := find(e.do("playlists"), "Mix"); row.Subtitle != "0 songs · On Q2" {
		t.Errorf("empty Mix %q", row.Subtitle)
	}
}

// Artists builds each row's status from one index of the manifest, not a walk of it per artist:
// thousands of artists over a big library still answer at once (I17).
func TestArtistsFastOnBigLibrary(t *testing.T) {
	d := newTestDaemon(t, &nav{artists: 4000}, "", true)
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	for i := 0; i < 40000; i++ {
		id := fmt.Sprintf("s%d", i)
		m.Songs[id] = &library.Entry{ID: id, Path: filepath.Join(d.card, "Subsonic", id+".flac"), Size: 1,
			Items: []string{fmt.Sprintf("album:al%d", i/10), "library:all"}}
	}
	if err := m.Save(d.card, filepath.Join(d.card, ".subsonic", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	d.do("onq2") // the manifest read once
	start := time.Now()
	r := d.do("artists")
	took := time.Since(start)
	if took > time.Second || len(r.Rows) != 50 {
		t.Fatalf("%d rows in %v", len(r.Rows), took)
	}
	t.Logf("%v", took)
}

// On This Q2 pages like every list: past 50 chosen items, More… (I21).
func TestOnQ2Pages(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 70, songsPer: 1}, "pages", true)
	sel := selection.Set{Version: 1}
	for i := 0; i < 70; i++ {
		sel.Toggle(selection.Item{Kind: selection.KindAlbum, ID: fmt.Sprintf("al%d", i), Name: fmt.Sprintf("Album %03d", i), Quality: "original"})
	}
	sel.Save(filepath.Join(d.dir, "selection.json"))
	r := d.do("onq2")
	if len(r.Rows) != 50 || r.Cursor == "" || r.Message != "pages" {
		t.Fatalf("%d rows, cursor %q, message %q", len(r.Rows), r.Cursor, r.Message)
	}
	if r = d.do("onq2", r.Cursor); len(r.Rows) != 20 || r.Rows[19].Title != "Album 069" {
		t.Fatalf("second page %q", titles(r))
	}
}

// Entire Library and a chosen song have their Quality in On This Q2 (I25).
func TestOnQ2Quality(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "", true)
	d.do("toggle", "song:al0.1 album:al0 1")
	r := d.do("onq2")
	q, ok := find(r, "Quality: Original")
	if !ok || q.Kind != "action" || q.URI != "quality" || q.Context != "song:al0.1" {
		t.Fatalf("song: %+v in %q", q, titles(r))
	}
	d.do("toggle", "song:al0.1 album:al0 1")
	d.do("toggle", "library:all")
	r = d.do("onq2")
	q, ok = find(r, "Quality: Original")
	if !ok || q.Kind != "action" || q.URI != "quality" || q.Context != "library:all" {
		t.Fatalf("library: %+v in %q", q, titles(r))
	}
	if r := d.do(q.URI, q.Context); r.Message != "Quality: MP3 320" {
		t.Fatalf("toast %q", r.Message)
	}
	if it := d.selected(t); it[0].Quality != "mp3-320" {
		t.Fatalf("%+v", it)
	}
}

// A page whose rows are too big for one 64 KiB reply ends where the reply ends, and the next page
// starts there: no row is skipped (M28).
func TestPageCutsBySize(t *testing.T) {
	var rows []q2spot.Row
	for i := 0; i < 60; i++ {
		rows = append(rows, list("album", fmt.Sprintf("%02d", i)+strings.Repeat("%", 250), strings.Repeat("%", 255), "album:x"))
	}
	r := page(rows, "")
	if _, kept := r.Encode("4294967295"); kept != len(r.Rows) || r.Cursor != strconv.Itoa(len(r.Rows)) {
		t.Fatalf("%d rows, %d sent, cursor %q", len(r.Rows), kept, r.Cursor)
	}
	r.Rows, r.Message = append([]q2spot.Row{searchRow, list("sync", "Sync Now", "999 waiting", "")}, r.Rows...), "pages"
	if _, kept := r.Encode("4294967295"); kept != len(r.Rows) {
		t.Fatalf("with Search and Sync Now: %d of %d sent", kept, len(r.Rows))
	}
}

// Failed Songs pages (M29).
func TestFailedSongsPages(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 60, mediaErr: 70}, "", true)
	d.runner.Backoff = []time.Duration{time.Millisecond}
	d.do("toggle", "album:al0")
	d.syncOK(t)
	r := d.do("failed")
	if len(r.Rows) != 50 || r.Cursor == "" {
		t.Fatalf("%d rows, cursor %q", len(r.Rows), r.Cursor)
	}
	if r = d.do("failed", r.Cursor); len(r.Rows) != 11 {
		t.Fatalf("second page %d rows", len(r.Rows))
	}
}

// A later page that fails while the firmware loads the list page by page is a failed reply, so the
// list keeps More… to try again; a first page still shows the message with Details (M30).
func TestLaterPageFails(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 120, songsPer: 1, listErr: 70}, "", true)
	r := d.do("albums")
	if r.Status != "" || r.Cursor == "" {
		t.Fatalf("first page %+v", r)
	}
	if r = d.do("albums", r.Cursor); r.Status == "" || r.Status == q2spot.OK || len(r.Rows) != 0 {
		t.Fatalf("later page %+v", r)
	}
}

// A removed item and a quality change wait for a sync, with Sync Now (M35).
func TestSyncNowForRemovalAndQuality(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 2}, "", true)
	d.do("toggle", "album:al0")
	d.do("toggle", "album:al1")
	d.syncOK(t)
	if _, ok := find(d.do("albums"), "Sync Now"); ok {
		t.Fatal("Sync Now after the sync")
	}
	d.do("toggle", "album:al0")
	d.do("quality", "album:al1")
	r := d.do("albums")
	if row, _ := find(r, "Sync Now"); row.Subtitle != "2 waiting" {
		t.Errorf("Sync Now %q in %q", row.Subtitle, titles(r))
	}
	for _, a := range []string{"Album 000", "Album 001"} {
		if row, _ := find(r, a); row.Subtitle != "Artist · Waiting to sync" {
			t.Errorf("%s %q", a, row.Subtitle)
		}
	}
}

// An artist that isn't chosen shows how much of it is on the Q2 through other items, once its page
// has been opened (M36).
func TestArtistPartialStatus(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 2}, "", true)
	d.do("toggle", "album:al0")
	d.syncOK(t)
	d.do("artist", "artist:ar1")
	if row, _ := find(d.do("artists"), "Artist"); row.Subtitle != "2 albums · 2 of 4 songs on Q2" {
		t.Errorf("artist %q", row.Subtitle)
	}
}

// With Entire Library on, an item's page says it is included that way instead of offering a
// removal that can't happen (M37).
func TestItemPageUnderLibrary(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 2}, "", true)
	d.do("toggle", "album:al2")
	d.do("toggle", "library:all")
	for _, a := range []string{"album:al2", "album:al1"} {
		r := d.do("album", a)
		if r.Rows[0].Title == "Sync Now" {
			r.Rows = r.Rows[1:]
		}
		if r.Rows[0].Kind != "info" || r.Rows[0].Title != "Included via Entire Library" {
			t.Errorf("%s: %+v", a, r.Rows[0])
		}
	}
}

// A synced song whose file was deleted from the card (by hand, or the folder after removing the
// app) is planned again, not taken as on the card (I9).
func TestDeletedFileFetchedAgain(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "", true)
	d.do("toggle", "album:al0")
	d.syncOK(t)
	for _, e := range d.manifest().Songs {
		os.Remove(e.Path)
		break
	}
	d.do("check")
	r := d.preview(t)
	if _, ok := find(r, "+1 song · "); !ok {
		t.Fatalf("preview %q", titles(r))
	}
	if n := len(d.manifest().Songs); n != 1 {
		t.Errorf("%d songs recorded", n)
	}
}

// A playlist change that moves no song still syncs: a new one whose songs are on the card, and one
// taken out whose songs stay (I10).
func TestPlaylistOnlyChangeSyncs(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "library", true)
	mix := filepath.Join(d.card, "_explaylist_data", "Mix.m3u")
	d.do("toggle", "album:al0")
	d.syncOK(t)
	seq, _ := d.libReq(t)
	d.answer(seq + " ok")
	d.tick()
	d.do("toggle", "playlist:pl1")
	d.do("check")
	r := d.preview(t)
	if _, ok := find(r, "Update 1 playlist"); !ok {
		t.Fatalf("preview %q", titles(r))
	}
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	if !exists(mix) {
		t.Fatal("Mix.m3u not written")
	}
	if r := d.do("sync"); r.Rows[0].Title != "Sync complete · Playlists updated · 0 failed" {
		t.Errorf("done %q", titles(r))
	}
	if _, lines := d.libReq(t); strings.Join(lines, "|") != "scan|add "+mix {
		t.Errorf("library request %q", lines)
	}
	d.do("check")
	if r := d.preview(t); r.Rows[0].Title != "Everything is up to date" {
		t.Fatalf("after the sync %q", titles(r))
	}
	d.do("toggle", "playlist:pl1")
	d.do("check")
	if _, ok := find(d.preview(t), "Update 1 playlist"); !ok {
		t.Fatal("taking it out is not offered")
	}
	d.do("start")
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" && !exists(mix) })
}

// A server that ignores the album list's offset ends Entire Library's listing once a page brings
// nothing new, instead of growing it until the memory runs out (M10).
func TestEntireLibraryPagingEnds(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 500, songsPer: 1, sameAlbums: true}, "", true)
	d.do("toggle", "library:all")
	d.preview(t)
	if n := len(d.daemon.preview.plan.Add); n != 500 {
		t.Errorf("Add %d, want 500", n)
	}
}

// Sign Out and Check Again cancel a preview still fetching (M11).
func TestPreviewCancelled(t *testing.T) {
	for _, op := range []string{"signout", "check"} {
		t.Run(op, func(t *testing.T) {
			n := &nav{albums: 1, songsPer: 1, holdAlbum: make(chan struct{})}
			d := newTestDaemon(t, n, "", true)
			sel := selection.Set{Version: 1}
			sel.Toggle(selection.Item{Kind: selection.KindAlbum, ID: "al0", Name: "Album 000", Quality: "original"})
			sel.Save(filepath.Join(d.dir, "selection.json"))
			d.do("sync")
			eventually(t, "the preview's request", func() bool { return n.albumReqs.Load() == 1 })
			d.do(op)
			eventually(t, "the request cancelled", func() bool { return n.albumGaveUp.Load() == 1 })
			close(n.holdAlbum)
		})
	}
}

// Sign Out cancels the sync: a later sign-in, maybe to another server, never resumes the old
// account's plan (M12).
func TestSignOutCancelsSync(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2, block: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	t.Cleanup(func() { close(n.block) })
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to run", d.runner.Busy)
	d.do("signout")
	eventually(t, "the sync to stop", func() bool { return !d.runner.Busy() })
	if st := d.runner.Snapshot().State; st != "idle" {
		t.Fatalf("state %q", st)
	}
}

// A damaged selection.json says so, with Reset Selection, instead of reading as empty (M21).
func TestDamagedSelection(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	os.WriteFile(filepath.Join(d.dir, "selection.json"), []byte(`{"items":[{"kind":`), 0o644)
	for _, op := range []string{"onq2", "sync"} {
		r := d.do(op)
		if r.Rows[0].Title != "Selection file is damaged" {
			t.Fatalf("%s: %q", op, titles(r))
		}
	}
	if r := d.do("toggle", "album:al0"); !strings.Contains(r.Message, "Selection file is damaged") {
		t.Errorf("toggle %q", r.Message)
	}
	reset, ok := find(d.do("onq2"), "Reset Selection")
	if !ok || reset.Kind != "action" {
		t.Fatalf("reset %+v", reset)
	}
	d.do(reset.URI, reset.Context)
	if _, err := os.Stat(filepath.Join(d.dir, "selection.json.bad")); err != nil {
		t.Fatal("damaged file not kept aside")
	}
	if r := d.do("onq2"); r.Rows[0].Title != "Nothing chosen yet" {
		t.Fatalf("after reset %q", titles(r))
	}
}

// A song in the way of the one-time move to Subsonic/ is named, with what to do and Try Again (M23).
func TestMigrateInTheWay(t *testing.T) {
	card, _ := legacyCard(t)
	m, _ := library.LoadManifest(filepath.Join(card, ".subsonic", "manifest.json"))
	var e *library.Entry
	for _, x := range m.Songs {
		e = x
		break
	}
	block := strings.Replace(e.Path, "/Navidrome/", "/Subsonic/", 1)
	os.MkdirAll(filepath.Dir(block), 0o755)
	os.WriteFile(block, []byte("mine"), 0o644)
	d := newTestDaemonOn(t, card, &nav{albums: 1, songsPer: 2}, "", true)
	r := d.do("sync")
	rel := strings.TrimPrefix(block, card+"/")
	if _, ok := find(r, rel+" is in the way"); !ok {
		t.Fatalf("rows %q", titles(r))
	}
	again, ok := find(r, "Try Again")
	if !ok || again.Kind != "action" {
		t.Fatalf("rows %q", titles(r))
	}
	os.Remove(block)
	d.do(again.URI, again.Context)
	if r := d.do("sync"); r.Rows[0].Title != "Checking server…" {
		t.Fatalf("after Try Again %q", titles(r))
	}
}

// A sync stopped by a card error says what it means in a sentence, with Details (M34).
func TestSyncStoppedDetails(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	os.WriteFile(filepath.Join(d.card, ".subsonic", "manifest.json"), []byte("{"), 0o644)
	d.do("start")
	eventually(t, "the sync to stop", func() bool { return d.runner.Snapshot().State == "error" })
	r := d.do("sync")
	if r.Rows[0].Title != "Sync stopped · Couldn't read the sync records on the card" {
		t.Errorf("rows %q", titles(r))
	}
	if _, ok := find(r, "Details"); !ok {
		t.Errorf("no Details: %q", titles(r))
	}
	if got := d.stopMessage(&os.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC}); got != "The card is full" {
		t.Errorf("ENOSPC: %q", got)
	}
}

// Choosing something while a sync runs keeps its summary for when it ends (M38).
func TestDonePageKeptAfterChoosingDuringSync(t *testing.T) {
	n := &nav{albums: 2, songsPer: 2, block: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to run", d.runner.Busy)
	d.do("toggle", "album:al1")
	close(n.block)
	eventually(t, "the sync", func() bool { return d.runner.Snapshot().State == "done" })
	if r := d.do("sync"); !strings.HasPrefix(r.Rows[0].Title, "Sync complete") {
		t.Fatalf("rows %q", titles(r))
	}
}

// What a crashed run left on stderr, and its exit code, which the launcher keeps in
// /tmp/q2-subsonic.crash, goes into log.txt at the next start (M43).
func TestCrashLogged(t *testing.T) {
	card, tmp := t.TempDir(), t.TempDir()
	crash := filepath.Join(tmp, "q2-subsonic.crash")
	os.WriteFile(crash, []byte("panic: boom\n\ngoroutine 1 [running]:\n"+strings.Repeat("main.f()\n", 2000)+"exit 2\n"), 0o644)
	newDaemon(filepath.Join(card, ".subsonic"), card, tmp)
	b, _ := os.ReadFile(filepath.Join(card, ".subsonic", "log.txt"))
	if !strings.Contains(string(b), "panic: boom") || !strings.Contains(string(b), "exit 2") || len(b) > 8<<10 {
		t.Fatalf("log %d bytes:\n%.300s", len(b), b)
	}
	if _, err := os.Stat(crash); err == nil {
		t.Fatal("crash file kept: logged again at every start")
	}
}

// A removal-only sync says what it did, not "0 songs" (review G3, Minor 7).
func TestDoneSaysRemoved(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "", true)
	d.do("toggle", "album:al0")
	d.syncOK(t)
	d.do("toggle", "album:al0")
	d.syncOK(t)
	if r := d.do("sync"); r.Rows[0].Title != "Sync complete · 2 songs removed · 0 failed" {
		t.Errorf("done %q", titles(r))
	}
}

// Details of a stopped sync carry when it stopped, not when they were opened, and its error
// (review G3, Minor 3).
func TestStoppedDetailsTime(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	os.WriteFile(filepath.Join(d.card, ".subsonic", "manifest.json"), []byte("{"), 0o644)
	d.do("start")
	eventually(t, "the sync to stop", func() bool { return d.runner.Snapshot().State == "error" })
	time.Sleep(1100 * time.Millisecond)
	det, _ := find(d.do("sync"), "Details")
	f := strings.Split(det.Context, "|")
	if unix, _ := strconv.ParseInt(f[3], 10, 64); unix >= time.Now().Unix() {
		t.Errorf("Details time %d is when they were opened", unix)
	}
	if got := strings.Join(titles(d.do(det.URI, det.Context)), "|"); !strings.Contains(got, "Error: ") {
		t.Errorf("no error in %q", got)
	}
}

// A resolved item is On Q2 only when its songs are: a song a server playlist dropped doesn't stand
// in for the one it gained (review G3, Minor 4).
func TestResolvedItemCountsItsOwnSongs(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 2}, "", true)
	d.do("toggle", "playlist:pl1")
	d.syncOK(t)
	d.mu.Lock()
	d.resolved["playlist:pl1"] = []subsonic.Song{{ID: "al0.0"}, {ID: "al1.0"}} // al0.1 swapped for al1.0
	d.mu.Unlock()
	r := d.do("playlists")
	if row, _ := find(r, "Mix"); row.Subtitle != "2 songs · 1 of 2 songs on Q2" {
		t.Errorf("Mix %q", row.Subtitle)
	}
	if _, ok := find(r, "Sync Now"); !ok {
		t.Error("no Sync Now")
	}
}

// A sync that ends paused or stopped counts as activity too: its reason stays on screen as long as
// a finished sync's summary (review G3, Minor 8).
func TestStaysUpAfterPausedSync(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2, block: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	t.Cleanup(func() { close(n.block) })
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to run", d.runner.Busy)
	d.tick()
	d.mu.Lock()
	d.lastReq = time.Now().Add(-time.Hour)
	d.mu.Unlock()
	d.runner.Pause() // as Wi-Fi going would
	d.tick()
	if d.idle(time.Now()) {
		t.Fatal("exits the moment the sync pauses")
	}
}

// libSongs is what Entire Library resolves to on n, and the daemon that did it.
func libSongs(t *testing.T, n *nav) ([]subsonic.Song, *tdaemon) {
	t.Helper()
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "library:all")
	if r := d.preview(t); len(r.Rows) == 0 || !strings.HasPrefix(r.Rows[0].Title, "+") {
		t.Fatalf("preview %q", titles(r))
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.resolved["library:all"], d
}

// Entire Library asks for the songs 500 at a time instead of album by album, and resolves to the
// same songs in the same order.
func TestEntireLibrarySongsInPages(t *testing.T) {
	plain := &nav{albums: 300, songsPer: 4}
	want, _ := libSongs(t, plain)
	if got := plain.albumReqs.Load(); got != 300 || len(want) != 1200 {
		t.Fatalf("a server without the empty query: %d album requests, %d songs", got, len(want))
	}
	n := &nav{albums: 300, songsPer: 4, paged: true}
	got, _ := libSongs(t, n)
	if a, s := n.albumReqs.Load(), n.songReqs.Load(); a != 0 || s != 3 {
		t.Errorf("%d album and %d page requests, want 0 and 3", a, s)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paged songs differ from the albums' (%d, %d)", len(got), len(want))
	}
}

// A server whose pages can't be trusted is checked album by album for what the pages left short;
// nothing is lost, and a refused query is not the preview's failure.
func TestEntireLibraryPagesDegrade(t *testing.T) {
	want, _ := libSongs(t, &nav{albums: 300, songsPer: 4})
	for name, c := range map[string]struct {
		n      *nav
		albums int32
	}{
		"offset ignored":  {&nav{paged: true, pageSame: true}, 175},
		"a page is empty": {&nav{paged: true, pageEmptyAt: 500}, 175},
		"query refused":   {&nav{paged: true, pageErr: 10}, 300},
	} {
		c.n.albums, c.n.songsPer = 300, 4
		got, _ := libSongs(t, c.n)
		if a := c.n.albumReqs.Load(); a != c.albums {
			t.Errorf("%s: %d album requests, want %d", name, a, c.albums)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: songs differ (%d, %d)", name, len(got), len(want))
		}
	}
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 2, paged: true, errCode: 10}, "", true)
	d.do("toggle", "library:all")
	if r := d.preview(t); len(d.do("removed").Rows) != 1 {
		t.Errorf("removals planned from a failed check")
	} else if _, ok := find(r, "Start Sync"); ok {
		t.Errorf("a failed check offers Start Sync: %q", titles(r))
	}
}

// A compilation from the pages stays one folder, as from getAlbum (M41).
func TestEntireLibraryPagedCompilation(t *testing.T) {
	got, _ := libSongs(t, &nav{albums: 2, songsPer: 3, paged: true, various: true})
	for _, s := range got {
		if s.AlbumArtist != "Various Artists" {
			t.Fatalf("%s: album artist %q", s.ID, s.AlbumArtist)
		}
	}
}

// again computes the preview anew, as opening Sync after previewTTL does.
func (d *tdaemon) again(t *testing.T) q2spot.Reply {
	t.Helper()
	d.mu.Lock()
	d.dropPreview()
	d.mu.Unlock()
	return d.preview(t)
}

func scanned(n *nav) *nav {
	n.lastScan.Store("2026-11-10T03:00:00Z")
	n.count.Store(int64(n.albums * n.songsPer))
	return n
}

// When the server says its library hasn't changed since the last check, the songs come from
// .subsonic/library.json and only the playlists are fetched.
func TestEntireLibraryCacheReused(t *testing.T) {
	n := scanned(&nav{albums: 3, songsPer: 2, paged: true})
	want, d := libSongs(t, n)
	file := filepath.Join(d.card, ".subsonic", "library.json")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	server := d.server
	d.resolved, d.libCount = map[string][]subsonic.Song{}, [2]int{}
	d.mu.Unlock()
	if u, _ := url.Parse(server); server == "" || strings.Contains(string(b), u.Host) || strings.Contains(string(b), `"u"`) {
		t.Errorf("the cache names the account (%s): %.200s", server, b)
	}
	r := d.again(t)
	if s, a := n.songReqs.Load(), n.albumReqs.Load(); s != 1 || a != 0 {
		t.Errorf("the second check made %d page and %d album requests", s-1, a)
	}
	if _, ok := find(r, "+6 songs"); !ok || len(d.daemon.preview.plan.Playlists["Mix"]) != 2 {
		t.Errorf("preview from the cache %q", titles(r))
	}
	d.mu.Lock()
	got := d.resolved["library:all"]
	d.mu.Unlock()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cached songs differ")
	}
	if row := d.do("menu").Rows[0]; row.Subtitle != "On · 3 albums · 1 playlist" {
		t.Errorf("Sync Entire Library %q", row.Subtitle)
	}

	// Anything that doesn't match is a full check, which leaves no older cache behind.
	edit := func(f func(c *libCache)) func() {
		return func() {
			var c libCache
			b, _ := os.ReadFile(file)
			if err := json.Unmarshal(b, &c); err != nil {
				t.Fatal(err)
			}
			f(&c)
			b, _ = json.Marshal(c)
			os.WriteFile(file, b, 0o644)
		}
	}
	for _, c := range []struct {
		name   string
		change func()
		writes bool
	}{
		{"lastScan moved", func() { n.lastScan.Store("2026-11-11T03:00:00Z") }, true},
		{"count changed", func() { n.count.Store(7) }, true},
		{"another account", func() { d.mu.Lock(); d.user = "other"; d.mu.Unlock() }, true},
		{"a day old", edit(func(c *libCache) { c.At = c.At.Add(-25 * time.Hour) }), true},
		{"from the future", edit(func(c *libCache) { c.At = c.At.Add(time.Hour) }), true},
		{"another format", edit(func(c *libCache) { c.Format++ }), true},
		{"damaged", func() { os.WriteFile(file, []byte(`{"Format":1,"Songs":[{`), 0o644) }, true},
		{"Check Again", func() { d.do("check") }, true},
		{"scanning", func() { n.scanning.Store(true) }, false},
	} {
		before := n.songReqs.Load()
		c.change()
		d.again(t)
		if n.songReqs.Load() == before {
			t.Errorf("%s: the cache was reused", c.name)
		}
		if exists(file) != c.writes {
			t.Errorf("%s: cache written %v, want %v", c.name, !c.writes, c.writes)
		}
		before = n.songReqs.Load()
		if d.again(t); c.writes && n.songReqs.Load() != before {
			t.Errorf("%s: the check after it didn't reuse its cache", c.name)
		}
	}
}

// The cache's day is measured by a clock that is known (the Q2's is often unset, and starts again at
// each boot): without one no cache is reused or written, and one written before the clock was known
// is not reused.
func TestEntireLibraryCacheNeedsClock(t *testing.T) {
	n := scanned(&nav{albums: 3, songsPer: 2, paged: true})
	_, d := libSongs(t, n)
	file := filepath.Join(d.card, ".subsonic", "library.json")
	boot := time.Date(2000, 1, 1, 0, 5, 0, 0, time.UTC)
	libNow = func() (time.Time, bool) { return boot, false }
	t.Cleanup(func() { libNow = libClock })
	before := n.songReqs.Load()
	if d.again(t); n.songReqs.Load() == before {
		t.Errorf("clock unset: the cache was reused")
	}
	if exists(file) {
		t.Errorf("clock unset: a cache was written")
	}
	// What an older version wrote at an unset clock, read at the same time of another boot.
	libNow = func() (time.Time, bool) { return boot, true }
	d.again(t)
	libNow = func() (time.Time, bool) { return boot.Add(time.Minute), false }
	before = n.songReqs.Load()
	if d.again(t); n.songReqs.Load() == before {
		t.Errorf("another boot: the cache was reused")
	}
}

// A check that is dropped while it asks for the scan status keeps the cache: only a full check that
// runs removes it.
func TestEntireLibraryCacheKeptWhenDropped(t *testing.T) {
	n := scanned(&nav{albums: 3, songsPer: 2, paged: true})
	_, d := libSongs(t, n)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := d.resolveLibrary(ctx, d.client(), make(chan struct{}, 4), func(string) {}, false)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a dropped check: %v", err)
	}
	if !exists(filepath.Join(d.card, ".subsonic", "library.json")) {
		t.Errorf("a dropped check removed the cache")
	}
}

// No cache is kept when the server can't vouch for it: no lastScan (not Navidrome), a scan that
// finished during the check, or a library too large to hold.
func TestEntireLibraryCacheNotWritten(t *testing.T) {
	moves := scanned(&nav{albums: 3, songsPer: 2, paged: true})
	moves.scanMoves = true
	for name, n := range map[string]*nav{
		"no lastScan":              {albums: 3, songsPer: 2, paged: true},
		"lastScan moved meanwhile": moves,
		"over 20,000 songs":        scanned(&nav{albums: 41, songsPer: 500, paged: true}),
	} {
		_, d := libSongs(t, n)
		if exists(filepath.Join(d.card, ".subsonic", "library.json")) {
			t.Errorf("%s: a cache was written", name)
		}
	}
	// The limit itself, maxCacheSongs, is still held.
	if _, d := libSongs(t, scanned(&nav{albums: maxCacheSongs / 500, songsPer: 500, paged: true})); !exists(filepath.Join(d.card, ".subsonic", "library.json")) {
		t.Errorf("%d songs: no cache was written", maxCacheSongs)
	}
}

// While it checks, the Sync page counts.
func TestSyncPageCounts(t *testing.T) {
	n := &nav{albums: 300, songsPer: 4, paged: true, holdSongs: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "library:all")
	if r := d.do("sync"); r.Message != "refresh" || len(r.Rows) != 1 || r.Rows[0].Subtitle != "" {
		t.Fatalf("first reply %+v", r)
	}
	eventually(t, "the count", func() bool {
		r := d.do("sync")
		return r.Message == "refresh" && r.Rows[0].Title == "Checking server…" && r.Rows[0].Subtitle == "500 of 1200 songs"
	})
	close(n.holdSongs)
	d.preview(t)
}

// After the songs the Sync page counts the playlists it has read.
func TestSyncPageCountsPlaylists(t *testing.T) {
	n := &nav{albums: 3, songsPer: 2, playlists: 3, holdPlaylists: make(chan struct{})}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "library:all")
	d.do("sync")
	eventually(t, "the count", func() bool {
		r := d.do("sync")
		return r.Message == "refresh" && r.Rows[0].Title == "Checking server…" && r.Rows[0].Subtitle == "1 of 3 playlists"
	})
	close(n.holdPlaylists)
	d.preview(t)
}

// Report Plays is on until turned off in Settings, which is kept and forgets the plays not sent.
func TestSettingsReportPlays(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "playlist", true)
	rows := titles(d.do("settings"))
	if len(rows) < 5 || rows[4] != "Report Plays: On" || !d.stream.plays.on() {
		t.Fatalf("rows %q", rows)
	}
	d.stream.plays.queue = []*play{{id: "s1"}}
	if r := d.do("report"); r.Message != "Report Plays: Off" || d.stream.plays.on() || len(d.stream.plays.queue) != 0 {
		t.Fatalf("%+v, queue %v", r, d.stream.plays.queue)
	}
	if b, _ := os.ReadFile(filepath.Join(d.dir, "settings.json")); !strings.Contains(string(b), `"noReport":true`) {
		t.Fatalf("settings.json %s", b)
	}
	if _, ok := find(d.do("settings"), "Report Plays: Off"); !ok {
		t.Fatalf("rows %q", titles(d.do("settings")))
	}
	if r := d.do("report"); r.Message != "Report Plays: On" || !d.stream.plays.on() {
		t.Fatalf("%+v", r)
	}
}

// A song the firmware says it plays reaches the server's scrobble, and the app stays for it.
func TestDaemonReportsPlay(t *testing.T) {
	n := &nav{albums: 1, songsPer: 1}
	d := newTestDaemon(t, n, "playlist", true)
	d.client() // signed in
	d.stream.plays.unit = time.Millisecond
	d.mu.Lock()
	d.lastReq = time.Now().Add(-2 * idleAfter)
	d.mu.Unlock()
	d.stream.plays.now(subsonic.Song{ID: "s1", Duration: 400}, 0)
	if d.idle(time.Now()) {
		t.Fatal("idle with a play pending")
	}
	until(t, "idle", func() bool { return d.idle(time.Now()) })
	if q := n.query("scrobble"); n.reqs("scrobble") != 2 || q.Get("id") != "s1" || q.Get("submission") != "true" || q.Get("time") == "" {
		t.Fatalf("%d scrobbles, the last %v", n.reqs("scrobble"), q)
	}
}

// "Last synced" is when the last sync finished, not when a check last rewrote the manifest; a card
// synced by an older build, with no record of it, shows the manifest's time as before.
func TestLastSynced(t *testing.T) {
	n := &nav{albums: 1, songsPer: 2}
	d := newTestDaemon(t, n, "", true)
	if at := d.lastSynced(); !at.IsZero() {
		t.Fatalf("never synced: %v", at)
	}
	d.do("toggle", "playlist:pl1")
	d.syncOK(t)
	rec, man := filepath.Join(d.card, ".subsonic", "synced"), filepath.Join(d.card, ".subsonic", "manifest.json")
	if at := d.lastSynced(); time.Since(at) > time.Minute || !exists(rec) {
		t.Fatalf("after a sync: %v", at)
	}
	// A check that rewrites the manifest (the album now holds the playlist's songs) starts no run.
	then := time.Now().Add(-26 * time.Hour).Truncate(time.Second)
	os.WriteFile(rec, []byte(then.Format(time.RFC3339)), 0o644)
	os.Chtimes(man, then, then)
	d.do("toggle", "album:al0")
	d.do("toggle", "playlist:pl1")
	d.do("check")
	d.preview(t)
	if st, _ := os.Stat(man); st.ModTime().Equal(then) {
		t.Fatal("the check didn't rewrite the manifest")
	}
	if at := d.lastSynced(); !at.Equal(then) {
		t.Errorf("after a check: %v, want %v", at, then)
	}
	d.do("start")
	eventually(t, "the sync to finish", func() bool { return d.runner.Snapshot().State == "done" && time.Since(d.lastSynced()) < time.Minute })
	// An older build's card: no record.
	os.Remove(rec)
	os.Chtimes(man, then, then)
	if at := d.lastSynced(); !at.Equal(then) {
		t.Errorf("no record: %v, want the manifest's %v", at, then)
	}
}

// After a restart, with nothing fetched yet, album rows read their status from the album ids the
// manifest records.
func TestAlbumStatusAfterRestart(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 3}, "", true)
	sel := selection.Set{Version: 1}
	sel.Toggle(selection.Item{Kind: selection.KindLibrary, ID: "all", Quality: "original"})
	sel.Save(filepath.Join(d.dir, "selection.json"))
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	for _, id := range []string{"al0.0", "al0.1", "al0.2", "al1.0"} {
		m.Songs[id] = &library.Entry{ID: id, Path: filepath.Join(d.card, "Subsonic", id+".flac"), Size: 1000, Quality: "original",
			Items: []string{"library:all"}, Album: id[:3]}
	}
	if err := m.Save(d.card, filepath.Join(d.card, ".subsonic", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	r := d.do("albums")
	for title, sub := range map[string]string{"Album 000": "Artist · On Q2", "Album 001": "Artist · 1 of 3 songs on Q2", "Album 002": "Artist"} {
		if row, _ := find(r, title); row.Subtitle != sub {
			t.Errorf("%s subtitle %q, want %q", title, row.Subtitle, sub)
		}
	}
	d.do("artist", "artist:ar1") // its albums, al0 and al1
	if row, _ := find(d.do("artists"), "Artist"); row.Subtitle != "2 albums · 4 of 6 songs on Q2" {
		t.Errorf("artist %q", row.Subtitle)
	}
}

// A manifest written before album ids were recorded loads, gets them at the next check, and is
// then left alone by the checks after it.
func TestAlbumBackfilledByCheck(t *testing.T) {
	n := &nav{albums: 2, songsPer: 2}
	d := newTestDaemon(t, n, "", true)
	d.do("toggle", "playlist:pl1")
	d.syncOK(t)
	mp := filepath.Join(d.card, ".subsonic", "manifest.json")
	m, _ := library.LoadManifest(mp)
	for _, e := range m.Songs {
		if e.Album != "al0" {
			t.Fatalf("synced: %+v", e)
		}
		e.Album = ""
	}
	m.Save(d.card, mp)
	if b, _ := os.ReadFile(mp); strings.Contains(string(b), `"Album"`) {
		t.Fatalf("an old manifest names no album: %s", b)
	}
	d2 := newTestDaemonOn(t, d.card, n, "", true)
	if row, _ := find(d2.do("albums"), "Album 000"); row.Subtitle != "Artist" {
		t.Errorf("before the check: %q", row.Subtitle)
	}
	d2.do("check")
	if r := d2.preview(t); len(r.Rows) != 1 || r.Rows[0].Title != "Everything is up to date" {
		t.Fatalf("preview %q", titles(r))
	}
	for _, e := range d2.manifest().Songs {
		if e.Album != "al0" {
			t.Errorf("after the check: %+v", e)
		}
	}
	then := time.Now().Add(-time.Hour).Truncate(time.Second)
	os.Chtimes(mp, then, then)
	d2.do("check")
	d2.preview(t)
	if st, _ := os.Stat(mp); !st.ModTime().Equal(then) {
		t.Error("a second check rewrote the manifest")
	}
	d3 := newTestDaemonOn(t, d.card, n, "", true)
	if row, _ := find(d3.do("albums"), "Album 000"); row.Subtitle != "Artist · On Q2" {
		t.Errorf("after a restart: %q", row.Subtitle)
	}
}

// Once the server has answered, the Sync page says what it does next instead of the last count,
// and each check leaves one line in the log with how long its parts took.
func TestSyncPageAfterFetches(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 2, playlists: 3}, "", true)
	hold := make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	defer release()
	d.free = func(string) int64 { <-hold; return 1 << 40 } // asked for once the fetches are done
	d.do("toggle", "library:all")
	d.do("sync")
	eventually(t, "the fetches to end", func() bool {
		r := d.do("sync")
		return r.Message == "refresh" && r.Rows[0].Title == "Checking server…" && r.Rows[0].Subtitle == "Working out the changes…"
	})
	release()
	d.preview(t)
	b, _ := os.ReadFile(filepath.Join(d.dir, "log.txt"))
	line := regexp.MustCompile(`(?m) sync: check: fetch \S+, library cache \S+, drop-missing \S+, plan \S+, refresh \S+, total \S+$`)
	if n := len(line.FindAll(b, -1)); n != 1 {
		t.Errorf("%d timing lines in the log:\n%s", n, b)
	}
}
