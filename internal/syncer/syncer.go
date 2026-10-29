// Package syncer runs a sync plan: renames, downloads (to .part, fsynced, renamed), covers,
// playlists, then removals (only once every download arrived). The manifest never names a file
// that isn't there; downloads are recorded in batches, so a crash re-fetches at most a batch. It
// pauses when the card goes, the server can't be reached or the card is full, and a plan run again
// continues where it stopped.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andsuor/q2-subsonic/internal/applog"
	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/plan"
	"github.com/andsuor/q2-subsonic/internal/q2"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

var ErrBusy = errors.New("a sync is already running")

// MediaSource is the part of *subsonic.Client the runner uses.
type MediaSource interface {
	MediaURL(id, quality string) string
	CoverURL(id string) string
	Get(ctx context.Context, u string, from int64) (*http.Response, error)
}

type Failure struct {
	SongID, Title, Op string // Op: download, cover or rename
	HTTP, Code        int
	At                time.Time
	Msg               string // redacted
}

type Snapshot struct {
	State                 string // idle, running, paused, done or error
	Done, Total, Failed   int
	BytesDone, BytesTotal int64
	Permille              int
	ETA                   time.Duration
	Current               string // "Artist – Title" of the song in flight
	Err                   error
	Failures              []Failure
}

type Runner struct {
	// Backoff is the wait after each failed try of a song; one try per entry.
	Backoff     []time.Duration
	CardMounted func() bool
	Now         func() time.Time
	Net         *NetSlot
	// OnDone is called, with no lock held, after each run that ends in state "done". It must not block.
	OnDone func(plan.Plan)

	root string
	src  MediaSource
	log  *applog.Log
	onOp func(op, path string) // tests: "download" and "remove", in order

	mu                    sync.Mutex
	plan                  *plan.Plan
	state, stopAs         string
	err                   error
	cancel                context.CancelFunc
	done                  chan struct{}
	doneN, total, failedN int
	bytesDone, bytesTotal int64
	cur, curMax           int64 // the file in flight: bytes on the card, its planned size
	current, part         string
	failures              []Failure
	meter                 meter
}

func New(root string, src MediaSource, log *applog.Log) *Runner {
	return &Runner{Backoff: []time.Duration{2 * time.Second, 8 * time.Second, 30 * time.Second},
		CardMounted: q2.CardMounted, Now: time.Now, Net: Net, root: root, src: src, log: log, state: "idle"}
}

func (r *Runner) manifestPath() string { return filepath.Join(r.root, ".subsonic", "manifest.json") }

func (r *Runner) logf(format string, a ...any) {
	if r.log != nil {
		r.log.Printf("%s", applog.Redact(fmt.Sprintf(format, a...)))
	}
}

// Start runs p in the background. Files already finished are skipped through the manifest, so a
// plan run again continues where it stopped.
func (r *Runner) Start(p plan.Plan) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == "running" {
		return ErrBusy
	}
	// Older builds kept the whole plan here, megabytes for an Entire Library that ran the app out of
	// memory (I16); nothing reads it.
	library.Remove(r.root, filepath.Join(r.root, ".subsonic", "pending.json"))
	r.plan = &p
	r.launch()
	return nil
}

// launch starts a run of r.plan; r.mu is held.
func (r *Runner) launch() {
	p := *r.plan
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.done = cancel, make(chan struct{})
	r.state, r.err, r.failures, r.meter = "running", nil, nil, meter{}
	r.doneN, r.failedN, r.total = 0, 0, len(p.Add)+len(p.Replace)
	r.bytesDone, r.bytesTotal, r.cur, r.curMax = 0, 0, 0, 0
	for _, w := range append(append([]plan.Want(nil), p.Add...), p.Replace...) {
		r.bytesTotal += w.Bytes
	}
	go r.run(ctx, p, r.done)
}

func (r *Runner) run(ctx context.Context, p plan.Plan, done chan struct{}) {
	defer close(done)
	err := r.work(ctx, p)
	finished := false
	defer func() { // deferred before the unlock below, so it runs after it
		if finished && r.OnDone != nil {
			r.OnDone(p)
		}
	}()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current = ""
	switch {
	case ctx.Err() != nil:
		r.state = r.stopAs
	case errors.Is(err, q2.ErrNoCard):
		r.state, r.err = "paused", q2.ErrNoCard
	case errors.Is(err, ErrOffline) || errors.Is(err, ErrCardFull):
		r.state, r.err = "paused", err // Resume, or the next Sync, continues once it is back
		r.logf("sync: paused: %v", err)
	case err != nil:
		r.state, r.err = "error", err
		r.logf("sync: %v", err)
	default:
		r.state, finished = "done", true
	}
}

func (r *Runner) stop(as string) {
	r.mu.Lock()
	if r.state != "running" {
		r.mu.Unlock()
		return
	}
	r.stopAs = as
	r.cancel()
	done := r.done
	r.mu.Unlock()
	<-done
}

// Pause stops after cutting the file in flight; its .part stays for Resume.
func (r *Runner) Pause() { r.stop("paused") }

// Resume continues a paused sync (paused by Pause or by the card going away).
func (r *Runner) Resume() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == "paused" && r.plan != nil {
		r.launch()
	}
}

// Cancel stops and forgets the plan. Finished files and their manifest entries stay.
func (r *Runner) Cancel() {
	r.stop("idle")
	r.mu.Lock()
	r.state, r.err, r.plan, r.current = "idle", nil, nil, ""
	part := r.part
	r.part = ""
	r.mu.Unlock()
	if part != "" {
		library.Remove(r.root, part)
		library.Remove(r.root, part+".key")
	}
}

// RetryFailed runs the last plan again once it has ended with failures; finished songs are skipped.
func (r *Runner) RetryFailed() {
	r.mu.Lock()
	p := r.plan
	ok := p != nil && (r.state == "error" || r.state == "done" && r.failedN > 0)
	r.mu.Unlock()
	if ok {
		r.Start(*p)
	}
}

func (r *Runner) Busy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state == "running"
}

func (r *Runner) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	done := r.bytesDone + min(r.cur, r.curMax)
	s := Snapshot{State: r.state, Done: r.doneN, Total: r.total, Failed: r.failedN, BytesDone: done,
		BytesTotal: r.bytesTotal, Current: r.current, Err: r.err, Failures: append([]Failure(nil), r.failures...)}
	switch {
	case s.State == "done":
		s.Permille = 1000
	case r.bytesTotal > 0:
		s.Permille = permille(done, r.bytesTotal)
	default:
		s.Permille = permille(int64(r.doneN), int64(r.total))
	}
	if s.State == "running" {
		s.ETA = r.meter.eta(r.Now(), r.bytesTotal-done)
	}
	return s
}

func (r *Runner) fail(id, title, op string, err error) {
	f := Failure{SongID: id, Title: title, Op: op, At: r.Now(), Msg: applog.Redact(err.Error())}
	var ae *subsonic.APIError
	if errors.As(err, &ae) {
		f.HTTP, f.Code = ae.HTTP, ae.Code
	}
	r.logf("sync: %s %s failed: %s", op, id, f.Msg)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, f)
	if op != "cover" {
		r.failedN++
	}
}

func (r *Runner) finished(w plan.Want) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.doneN++
	r.bytesDone += w.Bytes
	r.cur, r.curMax = 0, 0
}

// op tells tests what happened: download, remove, save, playlist, try.
func (r *Runner) op(op, path string) {
	if r.onOp != nil {
		r.onOp(op, path)
	}
}

// check is the gate before every file: stopped, or the card gone (USB storage took it).
func (r *Runner) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !r.CardMounted() {
		return q2.ErrNoCard
	}
	return nil
}

// cardErr blames a failed card write on the card when it has gone.
func (r *Runner) cardErr(err error) error {
	if err != nil && !r.CardMounted() {
		return q2.ErrNoCard
	}
	return err
}

func (r *Runner) work(ctx context.Context, p plan.Plan) error {
	m, err := library.LoadManifest(r.manifestPath())
	if err != nil {
		return r.cardErr(err)
	}
	unsaved, saved := 0, r.Now() // changes not on the card yet, and when it was last saved
	save := func() error {
		if !r.CardMounted() { // never MkdirAll .subsonic onto the Q2's own storage under the mount point
			return q2.ErrNoCard
		}
		r.op("save", "")
		if err := m.Save(r.root, r.manifestPath()); err != nil {
			return r.cardErr(err)
		}
		unsaved, saved = 0, r.Now()
		return nil
	}
	defer func() { // a stop keeps what is done; a crash re-fetches at most the songs since the last save
		if unsaved > 0 {
			save()
		}
	}()

	for _, mv := range p.Rename {
		if err := r.check(ctx); err != nil {
			return err
		}
		e := m.Songs[mv.ID]
		if e == nil || e.Path == mv.To {
			continue
		}
		if err := r.rename(e.Path, mv.To); err != nil {
			if !r.CardMounted() {
				return q2.ErrNoCard
			}
			r.fail(mv.ID, filepath.Base(mv.To), "rename", err)
			continue
		}
		// ponytail: e.Meta keeps the old MetaKey (Move has no Song); harmless, as Compute keys on the path.
		e.Path = mv.To
		if err := save(); err != nil {
			return err
		}
	}

	covers := map[string]bool{} // album folders whose cover was tried this run
	failed := false             // a wanted song is missing: nothing is removed this run
	streak := 0                 // songs failed in a row for the same reason as the whole sync would (I3)
	for _, w := range append(append([]plan.Want(nil), p.Add...), p.Replace...) {
		id := w.Song.ID
		if e := m.Songs[id]; e != nil && e.Path == w.Path && e.Quality == w.Quality && e.Meta == library.MetaKey(w.Song, w.Quality) {
			r.finished(w)
			continue
		}
		if err := r.check(ctx); err != nil {
			return err
		}
		r.mu.Lock()
		r.current = w.Song.Title
		if w.Song.Artist != "" {
			r.current = w.Song.Artist + " – " + w.Song.Title
		}
		r.mu.Unlock()
		n, err := r.fetchTries(ctx, w)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !r.CardMounted() {
				return q2.ErrNoCard
			}
			r.fail(id, w.Song.Title, "download", err)
			failed = true
			if stop := stopReason(err); stop == nil {
				streak = 0
			} else if streak++; streak >= 2 {
				return stop
			}
			continue
		}
		streak = 0
		old := m.Songs[id]
		e := &library.Entry{ID: id, Path: w.Path, Quality: w.Quality, Size: n, Items: w.Items,
			Meta: library.MetaKey(w.Song, w.Quality)}
		if w.Quality == "original" {
			e.SrcSize = w.Song.Size
		}
		m.Songs[id] = e
		unsaved++
		switch {
		case old != nil && !oneFile(old.Path, w.Path):
			if err := save(); err != nil { // the new file recorded before the old one goes
				return err
			}
			r.remove(old.Path)
		case unsaved >= saveEvery || r.Now().Sub(saved) >= saveAfter:
			if err := save(); err != nil {
				return err
			}
		}
		if dir := filepath.Dir(w.Path); !covers[dir] {
			covers[dir] = true
			r.cover(ctx, w)
		}
		r.finished(w)
		r.op("download", w.Path)
	}

	for name, f := range m.Playlists {
		switch {
		case !library.ValidPlaylistFile(r.root, f):
			r.logf("sync: ignoring bad recorded playlist path %q for %s", f, name)
			delete(m.Playlists, name)
		case strings.Contains(filepath.Base(f), ".."): // an older SafeName's; the firmware refuses it
			if m.Remove(r.root, f) == nil {
				delete(m.Playlists, name) // written again below under a clean name
			}
		}
	}
	planned := map[string]string{} // a path the plan gives a song -> its id
	for _, w := range append(append([]plan.Want(nil), p.Add...), p.Replace...) {
		planned[w.Path] = w.Song.ID
	}
	for _, mv := range p.Rename {
		planned[mv.To] = mv.ID
	}
	// Legacy "Navidrome - *.m3u" files are removed only in the sync after the one-time migration
	// (which sets PlaylistsStale); otherwise a file with that name is the user's own.
	migrating := m.PlaylistsStale
	names := make([]string, 0, len(p.Playlists))
	for name := range p.Playlists {
		names = append(names, name)
	}
	sort.Strings(names)
	// Every file is recorded, in one save, before any is written: the guard writes only recorded
	// files, and a stop part-way leaves at worst a recorded file not written yet, never a written
	// one a later sync takes for the user's own (and so writes a "(Subsonic)" copy beside).
	for _, name := range names {
		if m.Playlists[name] == "" {
			m.Playlists[name] = r.freePlaylistFile(m, name, false)
			unsaved++
		}
	}
	if unsaved > 0 {
		if err := save(); err != nil {
			return err
		}
	}
	for _, name := range names {
		if err := r.check(ctx); err != nil { // USB storage gives the app 3 s to let the card go
			return err
		}
		f := m.Playlists[name]
		have := []string{} // songs that failed are left out until they arrive
		for _, s := range p.Playlists[name] {
			if id, ok := planned[s]; ok && m.Songs[id] != nil {
				s = m.Songs[id].Path // where the song is: a failed rename or replace left it at its old path
			}
			if _, err := os.Stat(s); err == nil {
				have = append(have, s)
			}
		}
		if err := m.WriteM3U(r.root, f, have); err != nil {
			return r.cardErr(err)
		}
		r.op("playlist", f)
		if migrating {
			library.Remove(r.root, library.LegacyPlaylistFile(r.root, name))
		}
	}
	if !failed {
		for name, f := range m.Playlists {
			if _, ok := p.Playlists[name]; !ok {
				if m.Remove(r.root, f) == nil {
					delete(m.Playlists, name)
				}
			}
		}
	}
	if migrating {
		legacy, _ := filepath.Glob(filepath.Join(r.root, "_explaylist_data", "Navidrome - *.m3u"))
		for _, f := range legacy {
			library.Remove(r.root, f)
		}
	}
	m.PlaylistsStale = false
	if err := save(); err != nil {
		return err
	}

	if failed {
		return nil // I2: the card never ends with less than before; Retry Failed or the next Sync removes
	}
	keep := map[string]bool{} // files other songs stay in (one file under two names, from older builds)
	gone := map[string]bool{}
	for _, e := range p.Remove {
		gone[e.ID] = true
	}
	for id, e := range m.Songs {
		if !gone[id] {
			keep[strings.ToLower(e.Path)] = true
		}
	}
	for rest := p.Remove; len(rest) > 0; {
		if err := r.check(ctx); err != nil {
			return err
		}
		batch := rest[:min(len(rest), removeBatch)]
		rest = rest[len(batch):]
		var es []*library.Entry
		for _, pe := range batch {
			if e := m.Songs[pe.ID]; e != nil {
				es = append(es, e)
				delete(m.Songs, pe.ID)
			}
		}
		// Dropped from the manifest before the files go: a stop or crash part-way leaves at worst a
		// file the app forgot, never a song recorded as on the card that is not (I4).
		unsaved++
		if err := save(); err != nil {
			return err
		}
		for _, e := range es {
			if keep[strings.ToLower(e.Path)] {
				continue
			}
			if err := r.remove(e.Path); err != nil {
				r.logf("sync: remove %s: %v", e.ID, err)
				m.Songs[e.ID] = e // still there: the next sync tries again
				unsaved++
				continue
			}
			r.op("remove", e.Path)
		}
	}
	return save()
}

const (
	saveEvery   = 20               // downloads recorded per manifest save (M18) ...
	saveAfter   = 30 * time.Second // ... or this long since the last
	removeBatch = 100              // removals dropped from the manifest per save
)

// ErrOffline and ErrCardFull pause a sync: every song would fail the same way.
var (
	ErrOffline  = fmt.Errorf("lost the server: %w", subsonic.ErrUnreachable)
	ErrCardFull = errors.New("the card is full")
)

// stopReason tells a failure the whole sync shares (the connection, the card) from one of this
// song's own (an HTTP or Subsonic error for it): ErrOffline (wrapping err when it says more, such as
// a certificate or a changed password) or ErrCardFull; nil for the song's own.
func stopReason(err error) error {
	var ae *subsonic.APIError
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ENOSPC):
		return ErrCardFull
	case errors.As(err, &ae):
		if ae.HTTP == 0 || errors.Is(err, subsonic.ErrAuth) {
			return fmt.Errorf("%w: %w", ErrOffline, err)
		}
		return nil
	case errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF):
		return ErrOffline
	}
	return nil
}

// freePlaylistFile picks <name>.m3u (unless skipBase), else <name> (Subsonic).m3u, else
// <name> (Subsonic 2).m3u...: the first neither on the card (a user's own file) nor recorded for
// another playlist (case-folded: the card is FAT/exFAT).
func (r *Runner) freePlaylistFile(m *library.Manifest, name string, skipBase bool) string {
	base := strings.TrimSuffix(library.PlaylistFile(r.root, name), ".m3u")
	taken := func(f string) bool {
		if _, err := os.Lstat(f); err == nil {
			return true
		}
		for _, o := range m.Playlists {
			if strings.EqualFold(o, f) {
				return true
			}
		}
		return false
	}
	file := func(n int) string {
		switch n {
		case 0:
			return base + ".m3u"
		case 1:
			return base + " (Subsonic).m3u"
		}
		return fmt.Sprintf("%s (Subsonic %d).m3u", base, n)
	}
	n := 0
	if skipBase {
		n = 1
	}
	for taken(file(n)) {
		n++
	}
	return file(n)
}

// remove deletes path through the guard and, when its album folder is left holding only
// cover.jpg, the cover too, so the guard prunes the folder.
func (r *Runner) remove(path string) error {
	if err := library.Remove(r.root, path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if es, err := os.ReadDir(dir); err == nil && len(es) == 1 && es[0].Name() == "cover.jpg" {
		library.Remove(r.root, filepath.Join(dir, "cover.jpg"))
	}
	return nil
}

// oneFile reports whether a and b name one file: on the case-insensitive card, case variants do.
func oneFile(a, b string) bool {
	if a == b {
		return true
	}
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

// owned gates song writes (downloads and renames): only under Subsonic/, never .subsonic/.
func owned(root, path string) error {
	if !library.InSongs(root, path) {
		return errors.New("refusing song path outside Subsonic/: " + path)
	}
	return nil
}

func (r *Runner) rename(from, to string) error {
	for _, p := range []string{from, to} {
		if err := owned(r.root, p); err != nil {
			return err
		}
	}
	fi, ferr := os.Lstat(from)
	ti, terr := os.Lstat(to)
	switch {
	case ferr == nil && terr == nil && os.SameFile(fi, ti):
		// A case-only change on the case-insensitive card: one file. The rename may keep the old
		// case (FAT and casefold ignore it), and to reaches the file either way.
		os.Rename(from, to)
		return nil
	case terr == nil && errors.Is(ferr, os.ErrNotExist):
		return nil // moved already, and cut before the manifest was saved (M13)
	case terr == nil:
		return errors.New("rename target exists: " + to)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		return err
	}
	return r.remove(from) // the file is gone already; this prunes the folders it left empty
}

// fetchTries fetches w, one try per Backoff entry, waiting that long after each failed try but
// the last.
func (r *Runner) fetchTries(ctx context.Context, w plan.Want) (int64, error) {
	var err error
	for i, d := range r.Backoff {
		var n int64
		r.op("try", w.Path)
		if n, err = r.fetch(ctx, w); err == nil {
			return n, nil
		}
		if ctx.Err() != nil || !r.CardMounted() {
			return 0, err
		}
		r.logf("sync: %s try %d: %s", w.Song.ID, i+1, err)
		if i == len(r.Backoff)-1 {
			break
		}
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return 0, err
}

// fetch downloads w to <path>.part, resuming an original with Range when the server answers
// 206 at the right offset, then fsyncs and renames it into place. It returns the file's size.
func (r *Runner) fetch(ctx context.Context, w plan.Want) (int64, error) {
	if err := owned(r.root, w.Path); err != nil {
		return 0, err
	}
	part := w.Path + ".part"
	if err := os.MkdirAll(filepath.Dir(part), 0o755); err != nil {
		return 0, err
	}
	// The .part's key names the version it holds (quality, server size): another version's bytes
	// are never continued into a spliced song (M14). A .part with no key is an older build's.
	key, stamp := part+".key", fmt.Sprintf("%s %d", w.Quality, w.Bytes)
	r.mu.Lock()
	r.part = part // Cancel deletes it (and its key)
	r.mu.Unlock()
	var from int64
	// A .part already at the planned size (cut between fsync and rename, or an older, longer
	// version) is fetched again whole.
	if st, err := os.Stat(part); err == nil && w.Quality == "original" && (w.Bytes <= 0 || st.Size() < w.Bytes) {
		if b, err := os.ReadFile(key); errors.Is(err, os.ErrNotExist) || err == nil && string(b) == stamp {
			from = st.Size()
		}
	}
	if from == 0 {
		if err := q2.WriteFile(key, []byte(stamp)); err != nil {
			return 0, err
		}
	}
	if err := r.Net.Acquire(ctx, false); err != nil {
		return 0, err
	}
	held := true
	defer func() {
		if held {
			r.Net.Release()
		}
	}()
	// yield lets a priority waiter (the streamer) go first, then takes the slot back.
	yield := func() error {
		if !r.Net.PriorityWaiting() {
			return nil
		}
		r.Net.Release()
		held = false
		if err := r.Net.Acquire(ctx, false); err != nil {
			return err
		}
		held = true
		return nil
	}
	u := r.src.MediaURL(w.Song.ID, w.Quality)
	resp, err := r.src.Get(ctx, u, from)
	var ae *subsonic.APIError
	if from > 0 && errors.As(err, &ae) && ae.HTTP == http.StatusRequestedRangeNotSatisfiable {
		library.Remove(r.root, part)
		from = 0
		q2.WriteFile(key, []byte(stamp))
		resp, err = r.src.Get(ctx, u, 0)
	}
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if resp.StatusCode == http.StatusPartialContent {
		if from == 0 || !strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", from)) {
			library.Remove(r.root, part)
			return 0, fmt.Errorf("unexpected Content-Range %q", resp.Header.Get("Content-Range"))
		}
		flag = os.O_WRONLY | os.O_APPEND
	} else if from > 0 {
		from = 0 // 200: the server ignored Range, start over
		q2.WriteFile(key, []byte(stamp))
	}
	f, err := os.OpenFile(part, flag, 0o644)
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	r.cur, r.curMax = from, w.Bytes
	r.mu.Unlock()
	n, err := r.copy(f, resp.Body, from, yield)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(part, w.Path)
	}
	if err != nil {
		return 0, err
	}
	library.Remove(r.root, key)
	r.mu.Lock()
	r.part = ""
	r.mu.Unlock()
	return n, nil
}

// copy writes body to f, counting progress; n starts at from (the bytes already in the .part).
// Every 256 KiB it calls yield, so a stream never waits for a whole file.
func (r *Runner) copy(f *os.File, body io.Reader, n int64, yield func() error) (int64, error) {
	buf := make([]byte, 64<<10)
	since := 0
	for {
		if since >= 256<<10 {
			if err := yield(); err != nil {
				return n, err
			}
			since = 0
		}
		k, rerr := body.Read(buf)
		if k > 0 {
			if _, err := f.Write(buf[:k]); err != nil {
				return n, err
			}
			n += int64(k)
			since += k
			r.mu.Lock()
			r.cur = n
			r.meter.add(r.Now(), int64(k))
			r.mu.Unlock()
		}
		if rerr == io.EOF {
			return n, nil
		}
		if rerr != nil {
			return n, rerr
		}
	}
}

// cover writes cover.jpg into w's album folder when it is missing. A failure is recorded but
// does not fail the song.
func (r *Runner) cover(ctx context.Context, w plan.Want) {
	c := filepath.Join(filepath.Dir(w.Path), "cover.jpg")
	if w.Song.CoverArt == "" {
		return
	}
	if _, err := os.Stat(c); err == nil {
		return
	}
	err := func() error {
		if err := r.Net.Acquire(ctx, false); err != nil {
			return err
		}
		defer r.Net.Release()
		resp, err := r.src.Get(ctx, r.src.CoverURL(w.Song.CoverArt), 0)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return err
		}
		return q2.WriteFile(c, b)
	}()
	if err != nil && ctx.Err() == nil {
		r.fail(w.Song.ID, w.Song.Title, "cover", err)
	}
}

// NetSlot lets one network transfer run at a time. Priority waiters (the streamer) are served
// before normal ones (the sync runner, which takes it per file).
type NetSlot struct {
	mu         sync.Mutex
	held       bool
	prio, norm []chan struct{}
}

func NewNetSlot() *NetSlot { return &NetSlot{} }

// Net is the slot the sync runner and the streamer share.
var Net = NewNetSlot()

func (s *NetSlot) Acquire(ctx context.Context, priority bool) error {
	s.mu.Lock()
	if !s.held {
		s.held = true
		s.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	if priority {
		s.prio = append(s.prio, ch)
	} else {
		s.norm = append(s.norm, ch)
	}
	s.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		defer s.mu.Unlock()
		if !drop(&s.prio, ch) && !drop(&s.norm, ch) {
			s.handOff() // granted while giving up: pass it on
		}
		return ctx.Err()
	}
}

// PriorityWaiting reports whether a priority acquirer is queued.
func (s *NetSlot) PriorityWaiting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.prio) > 0
}

func (s *NetSlot) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handOff()
}

func (s *NetSlot) handOff() {
	for _, q := range []*[]chan struct{}{&s.prio, &s.norm} {
		if len(*q) > 0 {
			close((*q)[0])
			*q = (*q)[1:]
			return
		}
	}
	s.held = false
}

func drop(q *[]chan struct{}, ch chan struct{}) bool {
	for i, c := range *q {
		if c == ch {
			*q = append((*q)[:i], (*q)[i+1:]...)
			return true
		}
	}
	return false
}
