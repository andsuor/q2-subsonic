package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/syncer"
)

// picture is a plain w x h image: "jpeg", "png", or "gray" (a greyscale PNG).
func picture(format string, w, h int) []byte {
	var img image.Image = image.NewGray(image.Rect(0, 0, w, h))
	if format != "gray" {
		c := image.NewRGBA(image.Rect(0, 0, w, h))
		for i := range c.Pix {
			c.Pix[i] = 0x80
		}
		img = c
	}
	var b bytes.Buffer
	if format == "jpeg" {
		jpeg.Encode(&b, img, nil)
	} else {
		png.Encode(&b, img)
	}
	return b.Bytes()
}

// hugePNG is a small PNG whose header declares 20000 x 20000 px.
func hugePNG() []byte {
	b := picture("png", 8, 8)
	binary.BigEndian.PutUint32(b[16:], 20000)
	binary.BigEndian.PutUint32(b[20:], 20000)
	binary.BigEndian.PutUint32(b[29:], crc32.ChecksumIEEE(b[12:29]))
	return b
}

func coverName(uri, ctx string) string {
	return fmt.Sprintf("%08x.jpg", artKey(q2spot.Row{URI: uri, Context: ctx}))
}

func (d *tdaemon) artDir() string { return filepath.Join(d.tmp, "q2-subsonic.art") }

// covers is the names in the covers' directory, sorted.
func (d *tdaemon) covers() []string {
	es, _ := os.ReadDir(d.artDir())
	out := []string{}
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestArtKey(t *testing.T) { // the firmware's art_url: FNV-1a 32 over uri, a tab, context
	if got := coverName("album", "album:al1"); got != "a10cd198.jpg" {
		t.Fatal(got)
	}
	if got := coverName("playlist", "playlist:pl1"); got != "bd29bc09.jpg" {
		t.Fatal(got)
	}
}

// With "art", Albums opens at once saying "refresh", its covers land as small JPEGs under the
// firmware's names, and the next reply is the plain one; a later page never says "refresh".
func TestCoversBesideAlbums(t *testing.T) {
	n := &nav{albums: 120, songsPer: 1, art: true}
	d := newTestDaemon(t, n, "pages art", true)
	r := d.do("albums")
	if r.Message != "refresh" || len(r.Rows) != 50 || r.Cursor != "50" {
		t.Fatalf("%q %d %q", r.Message, len(r.Rows), r.Cursor)
	}
	d.bg.Wait()
	if got := d.covers(); len(got) != 50 || n.reqs("getCoverArt") != 50 || n.query("getCoverArt").Get("size") != "64" {
		t.Fatalf("%d files, %d requests, %v", len(got), n.reqs("getCoverArt"), n.query("getCoverArt"))
	}
	f, err := os.Open(filepath.Join(d.artDir(), coverName("album", "album:al0")))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if c, kind, err := image.DecodeConfig(f); err != nil || kind != "jpeg" || c.Width != 64 || c.Height != 64 {
		t.Fatal(c, kind, err)
	}
	after := d.do("albums")
	if after.Message != "pages" || !reflect.DeepEqual(after.Rows, r.Rows) {
		t.Fatalf("%q", after.Message)
	}
	plain := newTestDaemon(t, n, "pages", true) // the rows and the reply are those without "art"
	if !reflect.DeepEqual(plain.do("albums"), after) {
		t.Fatal("the reply with covers differs from the one without")
	}
	if p2 := d.do("albums", "50"); p2.Message != "pages" || len(p2.Rows) != 50 {
		t.Fatalf("%q %d", p2.Message, len(p2.Rows))
	}
	d.bg.Wait()
	if got := d.covers(); len(got) != 100 {
		t.Fatalf("%d files", len(got))
	}
	// one line per cover would push the sync's history out of log.txt
	if b, _ := os.ReadFile(filepath.Join(d.dir, "log.txt")); !strings.Contains(string(b), "getAlbumList2") || strings.Contains(string(b), "getCoverArt") {
		t.Fatalf("log:\n%s", b)
	}
}

func TestNoCoversWithoutArt(t *testing.T) {
	n := &nav{albums: 60, songsPer: 1, art: true}
	d := newTestDaemon(t, n, "pages window", true)
	for _, r := range []q2spot.Reply{d.do("albums"), d.do("playlists"), d.do("artist", "artist:ar1"), d.do("albumlist", "newest")} {
		if r.Message == "refresh" {
			t.Fatal("refresh without art")
		}
	}
	d.bg.Wait()
	if _, err := os.Stat(d.artDir()); n.reqs("getCoverArt") != 0 || !os.IsNotExist(err) {
		t.Fatal(n.reqs("getCoverArt"), err)
	}
	// a server that names no covers: plain rows, as before
	n2 := &nav{albums: 60, songsPer: 1}
	d2 := newTestDaemon(t, n2, "pages art", true)
	if r := d2.do("albums"); r.Message != "pages" {
		t.Fatal(r.Message)
	}
	d2.bg.Wait()
	if n2.reqs("getCoverArt") != 0 {
		t.Fatal(n2.reqs("getCoverArt"))
	}
}

// Playlists, an artist's albums, Discover's lists and a tab have covers; Search has none.
func TestCoversOtherLists(t *testing.T) {
	n := &nav{albums: 3, songsPer: 1, art: true, playlists: 2}
	plain := newTestDaemon(t, n, "pages sections", true)
	for _, c := range []struct {
		args []string
		file string
	}{{[]string{"playlists"}, coverName("playlist", "playlist:pl2")}, {[]string{"artist", "artist:ar1"}, coverName("album", "album:al1")},
		{[]string{"albumlist", "newest"}, coverName("album", "album:al2")}, {[]string{"albumlist", "random"}, coverName("album", "album:al2")},
		{[]string{"genre", "Rock"}, coverName("album", "album:al2")}, {[]string{"menu", "albums"}, coverName("album", "album:al2")}} {
		d := newTestDaemon(t, n, "pages art sections", true)
		if r := d.do(c.args[0], c.args[1:]...); r.Message != "refresh" {
			t.Fatalf("%v: %q", c.args, r.Message)
		}
		d.bg.Wait()
		r := d.do(c.args[0], c.args[1:]...)
		if want := plain.do(c.args[0], c.args[1:]...); r.Message == "refresh" || !reflect.DeepEqual(r, want) {
			t.Fatalf("%v: %+v, want %+v", c.args, r, want)
		}
		if _, err := os.Stat(filepath.Join(d.artDir(), c.file)); err != nil {
			t.Fatalf("%v: %v in %v", c.args, err, d.covers())
		}
		if r := d.do("search", "x"); r.Message == "refresh" {
			t.Fatal("search")
		}
	}
}

// A window's first reply is a first drawing too; a reply the firmware appends is not.
func TestCoversWindows(t *testing.T) {
	n := &nav{albums: 600, songsPer: 1, art: true}
	d := newTestDaemon(t, n, "pages art window", true)
	if r := d.do("albums", "48"); r.Message != "pages" {
		t.Fatal(r.Message)
	}
	if r := d.do("albums", "480"); r.Message != "refresh" || r.Rows[0].Kind != "page" {
		t.Fatalf("%q %+v", r.Message, r.Rows[0])
	}
}

// Whatever the server sends is stored as a small baseline JPEG or not at all, asked for once, and a
// cover that can't be used doesn't stop the ones after it.
func TestCoverFormats(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int{}
	bodies := map[string][]byte{
		"al0": picture("png", 64, 64),
		"al1": []byte("RIFF\x24\x00\x00\x00WEBPVP8 \x18\x00\x00\x00"),
		"al2": picture("jpeg", 300, 300),
		"al3": hugePNG(),
		"al4": append(picture("jpeg", 64, 64), make([]byte, 40<<10)...),
		"al7": picture("gray", 64, 48),
	}
	if c, _, err := image.DecodeConfig(bytes.NewReader(bodies["al3"])); err != nil || c.Width != 20000 {
		t.Fatal(c, err) // the header must read: its size is what refuses it
	}
	n := &nav{albums: 9, songsPer: 1, art: true}
	n.cover = func(id string, w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked[id]++
		mu.Unlock()
		switch id {
		case "al5":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"subsonic-response":{"status":"failed","error":{"code":70,"message":"Artwork not found"}}}`))
		case "al6":
			http.NotFound(w, r)
		case "al8":
			w.Write(picture("jpeg", 64, 64))
		default:
			w.Write(bodies[id])
		}
	}
	d := newTestDaemon(t, n, "pages art", true)
	d.do("albums")
	d.bg.Wait()
	want := []string{coverName("album", "album:al0"), coverName("album", "album:al7"), coverName("album", "album:al8")}
	got := d.covers()
	if len(got) != 3 {
		t.Fatalf("%v", got)
	}
	for _, name := range want {
		b, err := os.ReadFile(filepath.Join(d.artDir(), name))
		if err != nil || !bytes.HasPrefix(b, []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")) {
			t.Fatalf("%s: %v % x", name, err, b[:min(len(b), 12)])
		}
		img, kind, err := image.Decode(bytes.NewReader(b))
		if err != nil || kind != "jpeg" || img.Bounds().Dx() != 64 {
			t.Fatal(name, kind, err)
		}
		if _, ok := img.(*image.YCbCr); !ok { // three components, a greyscale source too
			t.Fatalf("%s: %T", name, img)
		}
	}
	if r := d.do("albums"); r.Message == "refresh" {
		t.Fatal("still refresh")
	}
	d.bg.Wait()
	mu.Lock()
	defer mu.Unlock()
	for i := 0; i < 9; i++ {
		if id := fmt.Sprintf("al%d", i); asked[id] != 1 {
			t.Fatalf("%s asked %d times", id, asked[id])
		}
	}
}

// A server that can't be reached drops the queue, with one line in the log; the covers are not
// misses, so the next visit asks again.
func TestCoverServerGone(t *testing.T) {
	n := &nav{albums: 10, songsPer: 1, art: true}
	n.cover = func(string, http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }
	d := newTestDaemon(t, n, "pages art", true)
	d.do("albums")
	d.bg.Wait()
	first := n.reqs("getCoverArt")
	if first == 0 || first > 2 || len(d.covers()) != 0 { // 2: Go asks again on a connection it reused
		t.Fatal(first, d.covers())
	}
	if b, _ := os.ReadFile(filepath.Join(d.dir, "log.txt")); strings.Count(string(b), "covers: ") != 1 {
		t.Fatalf("log:\n%s", b)
	}
	if r := d.do("albums"); r.Message != "refresh" {
		t.Fatal(r.Message)
	}
	d.bg.Wait()
	if n.reqs("getCoverArt") == first {
		t.Fatal("not asked again")
	}
}

// A list opened while another's covers still load has its own refresh phase.
func TestCoverSecondList(t *testing.T) {
	hold := make(chan struct{})
	n := &nav{albums: 10, songsPer: 1, art: true, playlists: 3}
	n.cover = func(id string, w http.ResponseWriter, r *http.Request) {
		select {
		case <-hold:
		case <-r.Context().Done():
		}
		w.Write(picture("jpeg", 64, 64))
	}
	d := newTestDaemon(t, n, "pages art", true)
	if d.do("albums").Message != "refresh" || d.do("playlists").Message != "refresh" || d.do("albums").Message != "refresh" {
		t.Fatal("no refresh")
	}
	close(hold)
	d.bg.Wait()
	if d.do("albums").Message == "refresh" || d.do("playlists").Message == "refresh" || len(d.covers()) != 13 {
		t.Fatal(d.covers())
	}
}

// Sign Out empties the covers, and a fetch that ends after it leaves nothing of the old account.
func TestCoverSignOut(t *testing.T) {
	hold := make(chan struct{})
	n := &nav{albums: 3, songsPer: 1, art: true}
	d := newTestDaemon(t, n, "pages art", true)
	d.do("albums")
	d.bg.Wait()
	if len(d.covers()) != 3 {
		t.Fatal(d.covers())
	}
	n.cover = func(id string, w http.ResponseWriter, r *http.Request) {
		select {
		case <-hold:
		case <-r.Context().Done():
		}
		w.Write(picture("jpeg", 64, 64))
	}
	n.playlists = 2
	d.do("playlists")
	eventually(t, "the cover's request", func() bool { return n.reqs("getCoverArt") == 4 })
	d.art.mu.Lock()
	gen := d.art.gen
	d.art.mu.Unlock()
	d.do("signout")
	close(hold)
	d.bg.Wait()
	d.art.done(gen, 7, picture("jpeg", 64, 64), nil)
	if _, err := os.Stat(d.artDir()); err != nil || len(d.covers()) != 0 {
		t.Fatal(err, d.covers())
	}
	d.close(filepath.Join(d.tmp, "sock"))
	if _, err := os.Stat(d.artDir()); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// A cover's file is named by its row's key alone, whatever the server calls the cover.
func TestCoverNameFromKey(t *testing.T) {
	n := &nav{albums: 1, songsPer: 1}
	d := newTestDaemon(t, n, "pages art", true)
	d.art.want([]q2spot.Row{{Kind: "list", URI: "album", Context: "album:al1"}}, map[string]string{"album:al1": "../../x"}, true)
	d.bg.Wait()
	if got := d.covers(); n.query("getCoverArt").Get("id") != "../../x" || len(got) != 1 || got[0] != "a10cd198.jpg" {
		t.Fatal(n.query("getCoverArt"), got)
	}
	for _, p := range []string{"x", "../x", "../../x", "x.jpg"} {
		if _, err := os.Stat(filepath.Join(d.artDir(), p)); !os.IsNotExist(err) {
			t.Fatal(p, err)
		}
	}
}

func TestCoverEviction(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "art", false)
	d.art.mu.Lock()
	for k := uint32(0); k < 1000; k++ {
		d.art.store(k, make([]byte, 5000)) // 8 KiB of tmpfs each
	}
	d.art.mu.Unlock()
	got := d.covers()
	if len(got) != artBudget/8192 || got[0] != fmt.Sprintf("%08x.jpg", 1000-artBudget/8192) || got[len(got)-1] != "000003e7.jpg" {
		t.Fatal(len(got), got[0], got[len(got)-1])
	}
	d.art.mu.Lock()
	defer d.art.mu.Unlock()
	if d.art.have[0] || !d.art.have[999] {
		t.Fatal("the oldest is still counted")
	}
}

// While a sync holds the network a cover waits, however long, and is neither lost nor a miss; the
// list answers at once and stops saying "refresh" once the wait is over.
func TestCoverWaitsForSync(t *testing.T) {
	n := &nav{albums: 5, songsPer: 1, art: true}
	d := newTestDaemon(t, n, "pages art", true)
	d.art.fetchIn, d.art.wait = time.Second, 200*time.Millisecond
	if err := syncer.Net.Acquire(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r := d.do("albums")
	if r.Message != "refresh" || time.Since(start) > time.Second {
		syncer.Net.Release()
		t.Fatal(r.Message, time.Since(start))
	}
	time.Sleep(1200 * time.Millisecond) // longer than a fetch may take
	r, asked := d.do("albums"), n.reqs("getCoverArt")
	syncer.Net.Release()
	if r.Message == "refresh" || asked != 0 {
		t.Fatal(r.Message, asked)
	}
	d.bg.Wait()
	if len(d.covers()) != 5 {
		t.Fatal(d.covers())
	}
}

// A wait that ran out with covers still to come is over: the list opened again has a refresh phase
// of its own, so the covers fetched meanwhile are drawn on that visit.
func TestCoverRevisitAfterWait(t *testing.T) {
	n := &nav{albums: 5, songsPer: 1, art: true}
	d := newTestDaemon(t, n, "pages art", true)
	d.art.wait = 100 * time.Millisecond
	if err := syncer.Net.Acquire(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	first := d.do("albums").Message
	time.Sleep(150 * time.Millisecond)
	poll, again := d.do("albums").Message, d.do("albums").Message
	syncer.Net.Release()
	if first != "refresh" || poll == "refresh" || again != "refresh" {
		t.Fatalf("%q, %q, %q", first, poll, again)
	}
	d.bg.Wait()
}

// The pages the firmware appends by itself wait behind the covers of the rows on screen.
func TestCoverPagesBehind(t *testing.T) {
	hold := make(chan struct{})
	var mu sync.Mutex
	var order []string
	n := &nav{albums: 1, songsPer: 1}
	n.cover = func(id string, w http.ResponseWriter, r *http.Request) {
		<-hold
		mu.Lock()
		order = append(order, id)
		mu.Unlock()
		w.Write(picture("jpeg", 64, 64))
	}
	d := newTestDaemon(t, n, "pages art", true)
	rows := func(ids ...string) (rs []q2spot.Row, covers map[string]string) {
		covers = map[string]string{}
		for _, id := range ids {
			rs, covers["album:"+id] = append(rs, q2spot.Row{Kind: "list", URI: "album", Context: "album:" + id}), id
		}
		return
	}
	r1, c1 := rows("a1", "a2", "a3")
	r2, c2 := rows("b1", "b2", "a3")
	if !d.art.want(r1, c1, true) || d.art.want(r2, c2, false) {
		t.Fatal("refresh")
	}
	close(hold)
	d.bg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(order, " "); got != "a1 a2 a3 b1 b2" {
		t.Fatal(got)
	}
}

// A proxy answering for a server that is away (502, 503, 504) or a throttle (429) is no miss: the
// queue is dropped and the next visit asks again.
func TestCoverServerAway(t *testing.T) {
	for _, code := range []int{429, 502, 503, 504} {
		var away sync.Map
		away.Store("on", true)
		n := &nav{albums: 4, songsPer: 1, art: true}
		n.cover = func(id string, w http.ResponseWriter, r *http.Request) {
			if _, on := away.Load("on"); on {
				w.WriteHeader(code)
				return
			}
			w.Write(picture("jpeg", 64, 64))
		}
		d := newTestDaemon(t, n, "pages art", true)
		d.do("albums")
		d.bg.Wait()
		if got := n.reqs("getCoverArt"); got != 1 || len(d.covers()) != 0 {
			t.Fatal(code, got, d.covers())
		}
		away.Delete("on")
		if r := d.do("albums"); r.Message != "refresh" {
			t.Fatal(code, r.Message)
		}
		d.bg.Wait()
		if len(d.covers()) != 4 {
			t.Fatal(code, d.covers())
		}
	}
}
