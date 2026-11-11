package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	"image/draw"
	"image/jpeg"
	_ "image/png" // a playlist's cover is a PNG on Navidrome
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/andsuor/q2-subsonic/internal/applog"
	"github.com/andsuor/q2-subsonic/internal/q2"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
	"github.com/andsuor/q2-subsonic/internal/syncer"
)

// Covers beside rows (Q2 Pod's "art" word, q2-pod patch/addon.c). The firmware shows the JPEG at
// /tmp/q2-subsonic.art/<key>.jpg beside the row with that key, 64 px square, when the file is there
// as the list is drawn; it never asks for one. So no reply waits for a cover: a list's first reply
// says "refresh" while its covers load, and the plain reply after it makes the firmware draw the
// list once more, with them. Rows the firmware appends later (its pages) show theirs from the next
// visit on. The files are in RAM (tmpfs), so they are few and small.
const (
	artPx     = 64       // asked of the server: what the firmware draws
	artMaxPx  = 128      // the firmware takes a larger file for none
	artBody   = 32 << 10 // a cover's bytes at most; a server ignoring the size sends far more
	artBudget = 3 << 20  // of /tmp for the files, each counted in whole 4 KiB blocks
	artQueue  = 500      // covers waiting at most: the list opened last goes first
	artWait   = 6 * time.Second
	artFetch  = 5 * time.Second
)

// jfif is the JFIF header Go's encoder leaves out, for a decoder that looks for it.
var jfif = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00")

// artKey is the firmware's name for a row's cover (art_url): FNV-1a 32 over uri, a tab, context.
func artKey(row q2spot.Row) uint32 {
	h := fnv.New32a()
	h.Write([]byte(row.URI + "\t" + row.Context))
	return h.Sum32()
}

type artJob struct {
	key uint32
	id  string // the server's coverArt
}

type artFile struct {
	key  uint32
	size int64
}

// artCache fetches covers one at a time in the background and keeps their files within artBudget.
// ponytail: a cover is kept by its row, not by the server's cover id, so one changed on the server
// stays until Sign Out or the app's next start; key the files' freshness by id if that matters.
type artCache struct {
	d       *daemon
	dir     string
	net     *syncer.NetSlot
	wait    time.Duration // how long a first reply says "refresh" for its covers
	fetchIn time.Duration // one cover's request

	mu      sync.Mutex
	queue   []artJob
	flying  *artJob   // being fetched: no longer queued, not yet stored
	running bool      // drain is
	files   []artFile // oldest first
	have    map[uint32]bool
	miss    map[uint32]bool // no usable cover: not asked again. ponytail: unbounded, 4 bytes per row browsed
	total   int64
	burst   uint32    // the first cover row of the list whose refresh phase runs, since burstAt
	burstAt time.Time // zero: none runs
	gen     int       // bumped by clear: a fetch begun before it stores nothing
	ctx     context.Context
	cancel  context.CancelFunc
}

// newArtCache starts with no files: an app that was killed left its own.
func newArtCache(d *daemon, dir string) *artCache {
	c := &artCache{d: d, dir: dir, net: syncer.Net, wait: artWait, fetchIn: artFetch}
	c.reset(d.caps.Art)
	return c
}

// reset forgets every cover and stops the fetches; keep leaves the empty directory for new ones.
func (c *artCache) reset(keep bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.gen++
	c.queue, c.files, c.total, c.burstAt = nil, nil, 0, time.Time{}
	c.have, c.miss = map[uint32]bool{}, map[uint32]bool{}
	os.RemoveAll(c.dir)
	if keep {
		os.MkdirAll(c.dir, 0o755)
	}
}

// clear is Sign Out: the next account never sees this one's covers.
func (c *artCache) clear() { c.reset(c.d.caps.Art) }

// remove is the app's end.
func (c *artCache) remove() { c.reset(false) }

// want queues the covers rows lack (covers: a row's context to the server's cover id) ahead of
// those waiting, and reports whether a first reply should say "refresh" for them: some are still
// to come, and this list's wait is not over. The wait starts when a list's covers are first wanted
// and a poll of the same list doesn't start it again; another list does.
func (c *artCache) want(rows []q2spot.Row, covers map[string]string, first bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	var jobs []artJob
	var head uint32 // the first row with a cover
	seen, pending, asked := false, false, map[uint32]bool{}
	for _, r := range rows {
		id := covers[r.Context]
		if r.Kind != "list" || id == "" {
			continue
		}
		k := artKey(r)
		if !seen {
			seen, head = true, k
		}
		if c.have[k] || c.miss[k] || asked[k] {
			continue
		}
		pending, asked[k] = true, true
		if c.flying == nil || c.flying.key != k {
			jobs = append(jobs, artJob{k, id})
		}
	}
	if !pending {
		if first {
			c.burstAt = time.Time{}
		}
		return false
	}
	for _, j := range c.queue {
		if !asked[j.key] && len(jobs) < artQueue {
			jobs = append(jobs, j)
		}
	}
	c.queue = jobs
	if !c.running && len(c.queue) > 0 {
		c.running = true
		c.d.spawn(c.drain)
	}
	if !first {
		return false
	}
	if c.burstAt.IsZero() || c.burst != head {
		c.burst, c.burstAt = head, time.Now()
	}
	return time.Since(c.burstAt) < c.wait
}

// drain fetches the queue, one cover at a time, until it is empty.
func (c *artCache) drain() {
	for {
		c.mu.Lock()
		c.flying = nil
		if len(c.queue) == 0 {
			c.running = false
			c.mu.Unlock()
			return
		}
		j := c.queue[0]
		c.queue, c.flying = c.queue[1:], &j
		gen, ctx := c.gen, c.ctx
		c.mu.Unlock()
		b, err := c.fetch(ctx, j.id)
		c.done(gen, j.key, b, err)
	}
}

// errNoCover is a cover the Q2 can't show: not asked for again.
var errNoCover = errors.New("no usable cover")

// done takes a fetch's end: the cover stored, a miss noted, or, when the server can't be reached,
// the queue dropped (the next visit asks again). Nothing after a clear: ids repeat across servers.
func (c *artCache) done(gen int, key uint32, b []byte, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ae *subsonic.APIError
	switch {
	case c.gen != gen:
	case err == nil:
		c.store(key, b)
	case err == errNoCover || errors.As(err, &ae) && ae.HTTP != 0: // the server answered, whatever it said
		c.miss[key] = true
	default:
		c.d.log.Printf("covers: %d dropped: %s", len(c.queue)+1, applog.Redact(err.Error()))
		c.queue = nil
	}
}

// fetch is the cover id as the bytes to store. It waits for the network slot as long as a sync or
// a stream holds it; only the request itself is timed.
func (c *artCache) fetch(ctx context.Context, id string) ([]byte, error) {
	cl := c.d.client()
	if cl == nil {
		return nil, errSignedOut
	}
	if err := c.net.Acquire(ctx, false); err != nil {
		return nil, err
	}
	defer c.net.Release()
	quiet := *cl
	quiet.Log = nil // a line per cover would push the sync's history out of log.txt
	ctx, cancel := context.WithTimeout(ctx, c.fetchIn)
	defer cancel()
	resp, err := quiet.Get(ctx, quiet.CoverURLSize(id, artPx), 0)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, artBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > artBody {
		return nil, errNoCover
	}
	return baseline(b)
}

// baseline is picture b (a JPEG or PNG of at most artMaxPx either way) as a plain three-component
// baseline JPEG, the one kind the firmware's decoder, which runs on its UI thread, is given
// whatever the server sent. The size is read before anything is decoded: a small file can declare
// thousands of pixels.
func baseline(b []byte) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > artMaxPx || cfg.Height > artMaxPx {
		return nil, errNoCover
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, errNoCover
	}
	rgba := image.NewRGBA(image.Rect(0, 0, cfg.Width, cfg.Height))
	draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
	out := bytes.NewBuffer(append([]byte(nil), jfif...))
	var enc bytes.Buffer
	if err := jpeg.Encode(&enc, rgba, &jpeg.Options{Quality: 80}); err != nil {
		return nil, errNoCover
	}
	out.Write(enc.Bytes()[2:]) // past its own start-of-image marker
	return out.Bytes(), nil
}

// store writes a cover's file, whole then renamed as the firmware asks, and drops the oldest ones
// past artBudget. Not a card write: the name is the key's, under the app's own folder in /tmp.
// c.mu is held.
func (c *artCache) store(key uint32, b []byte) {
	name := func(k uint32) string { return filepath.Join(c.dir, fmt.Sprintf("%08x.jpg", k)) }
	if err := q2.WriteFile(name(key), b); err != nil {
		c.miss[key] = true // /tmp is full or gone: not tried again and again
		return
	}
	size := (int64(len(b)) + 4095) &^ 4095
	c.files, c.have[key], c.total = append(c.files, artFile{key, size}), true, c.total+size
	for c.total > artBudget {
		old := c.files[0]
		c.files, c.total = c.files[1:], c.total-old.size
		delete(c.have, old.key)
		os.Remove(name(old.key))
	}
}

// withArt asks for the covers of r's rows (covers: a row's context to the server's cover id), and
// has a first drawing (a list's first reply, or a window's) say "refresh" while they load. Without
// the "art" word, or a server naming no covers, r is as it was.
func (d *daemon) withArt(r q2spot.Reply, covers map[string]string, cursor string) q2spot.Reply {
	if !d.caps.Art || r.Status != "" || len(covers) == 0 {
		return r
	}
	off, _ := strconv.Atoi(cursor)
	if d.art.want(r.Rows, covers, off <= 0 || d.caps.Window && off%winRows == 0) {
		r.Message = "refresh"
	}
	return r
}
