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
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
	ErrAt                 time.Time // when the run stopped with Err
	Renamed, Removed      int       // songs this run renamed and removed
	Failures              []Failure
}

type Runner struct {
	// Backoff is the wait after each failed try of a song; one try per entry.
	Backoff     []time.Duration
	CardMounted func() bool
	Now         func() time.Time
	Net         *NetSlot
	// OnDone is called, with no lock held, after each run that ends in state "done". It must not
	// block. The runner is Busy until it returns.
	OnDone func(plan.Plan)

	root string
	src  MediaSource
	log  *applog.Log
	onOp func(op, path string) // tests: "download" and "remove", in order

	mu                    sync.Mutex
	plan                  *plan.Plan
	state, stopAs         string
	ending                bool // a run is done, and its OnDone hasn't returned yet
	err                   error
	cancel                context.CancelFunc
	done                  chan struct{}
	doneN, total, failedN int
	renamedN, removedN    int
	errAt                 time.Time
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
	r.doneN, r.failedN, r.total, r.renamedN, r.removedN = 0, 0, len(p.Add)+len(p.Replace), 0, 0
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
		r.mu.Lock()
		r.ending = false
		r.mu.Unlock()
	}()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current = ""
	switch {
	case ctx.Err() != nil:
		r.state = r.stopAs
	case errors.Is(err, q2.ErrNoCard):
		r.state, r.err, r.errAt = "paused", q2.ErrNoCard, r.Now()
	case errors.Is(err, ErrOffline) || errors.Is(err, ErrCardFull) || errors.Is(err, ErrRefused):
		r.state, r.err, r.errAt = "paused", err, r.Now() // Resume, or the next Sync, continues once it is back
		r.logf("sync: paused: %v", err)
	case err != nil:
		r.state, r.err, r.errAt = "error", err, r.Now()
		r.logf("sync: %v", err)
	default:
		r.state, finished, r.ending = "done", true, true
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

// Refresh records keep (plan.Keep: the items holding songs that need no transfer) while no sync
// runs, so an up-to-date preview still updates what each song is on the card for (I12). ErrBusy
// while one runs; that run records them itself.
func (r *Runner) Refresh(keep []plan.Want) error {
	if len(keep) == 0 {
		return nil
	}
	return r.edit(func(m *library.Manifest) (bool, error) { return applyKeep(m, keep), nil })
}

// Imported takes the firmware's answer to a library request that imported sent[0], added sent[1],
// and skipped exists (R35). What it imported or added is now a Q2 playlist the app made, so later
// requests replace it by name. A skipped add still under its playlist's own name met a playlist
// the user made: it is renamed "<name> (Subsonic).m3u" and returned in again, to add once more.
// One skipped under that name too is a clash. Only an exists naming exactly an add of the request
// counts; any other is logged and ignored, and then no add is recorded (one may have been
// skipped: the guard fails closed). ErrBusy while a sync runs.
func (r *Runner) Imported(sent [2][]string, exists []string) (again, clash []string, err error) {
	imports, adds := sent[0], sent[1]
	added, skipped, trusted := map[string]bool{}, map[string]bool{}, true
	for _, f := range adds {
		added[f] = true
	}
	for _, f := range exists {
		if added[f] {
			skipped[f] = true
			continue
		}
		r.logf("library: ignoring \"exists %s\": not an add of this request", f)
		trusted = false
	}
	err = r.edit(func(m *library.Manifest) (bool, error) {
		changed := false
		for _, f := range append(append([]string(nil), imports...), adds...) {
			if (!added[f] || trusted && !skipped[f]) && !m.Imported[f] {
				m.Imported[f], changed = true, true
			}
		}
		names := make([]string, 0, len(m.Playlists))
		for name := range m.Playlists {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			f := m.Playlists[name]
			switch {
			case !skipped[f]:
			case f != library.PlaylistFile(r.root, name):
				clash = append(clash, f)
			default:
				to := r.freePlaylistFile(m, name, true)
				if err := m.MovePlaylist(r.root, name, to); err != nil {
					return changed, err
				}
				again, changed = append(again, to), true
			}
		}
		return changed, nil
	})
	return again, clash, err
}

// edit changes the manifest while no sync runs: a run holds it in memory and saves it whole.
func (r *Runner) edit(f func(*library.Manifest) (bool, error)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == "running" {
		return ErrBusy
	}
	m, err := library.LoadManifest(r.manifestPath())
	if err != nil {
		return err
	}
	changed, err := f(m)
	if changed {
		if serr := m.Save(r.root, r.manifestPath()); err == nil {
			err = serr
		}
	}
	return err
}

// applyKeep records each kept song's items and MetaKey, once it is where the plan puts it (a
// rename that failed is left for the next sync).
func applyKeep(m *library.Manifest, keep []plan.Want) bool {
	changed := false
	for _, w := range keep {
		e := m.Songs[w.Song.ID]
		if e == nil || e.Path != w.Path || e.Quality != w.Quality {
			continue
		}
		if meta := library.MetaKey(w.Song, w.Quality); e.Meta != meta || !slices.Equal(e.Items, w.Items) {
			e.Meta, e.Items, changed = meta, w.Items, true
		}
	}
	return changed
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

// DropMissing forgets the songs whose files are gone from the card (deleted by hand, or with the
// folder after the app was removed), so the next plan fetches them again (I9). The card is read
// without the lock, which a reply may wait for; nothing is dropped while the card is away, when
// every file would seem gone. ErrBusy while a sync runs.
func (r *Runner) DropMissing() error {
	if !r.CardMounted() {
		return q2.ErrNoCard
	}
	m, err := library.LoadManifest(r.manifestPath())
	if err != nil {
		return err
	}
	gone := map[string]string{} // id -> its path
	for id, e := range m.Songs {
		if _, err := os.Lstat(e.Path); errors.Is(err, os.ErrNotExist) {
			gone[id] = e.Path
		}
	}
	if len(gone) == 0 || !r.CardMounted() {
		return nil
	}
	return r.edit(func(m *library.Manifest) (bool, error) {
		n := 0
		for id, path := range gone {
			if e := m.Songs[id]; e != nil && e.Path == path {
				delete(m.Songs, id)
				n++
			}
		}
		r.logf("sync: %d songs gone from the card, planned again", n)
		return n > 0, nil
	})
}

// Moving reports whether the plan a sync runs, or holds paused, renames, replaces or removes song
// id: the streamer then fetches the song rather than queue a path that is about to go (M24).
func (r *Runner) Moving(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.plan; p != nil && (r.state == "running" || r.state == "paused") {
		for _, mv := range p.Rename {
			if mv.ID == id {
				return true
			}
		}
		for _, w := range p.Replace {
			if w.Song.ID == id {
				return true
			}
		}
		for _, e := range p.Remove {
			if e.ID == id {
				return true
			}
		}
	}
	return false
}

// Busy is whether a sync runs, or has just ended and its OnDone hasn't returned (I15).
func (r *Runner) Busy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state == "running" || r.ending
}

func (r *Runner) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	done := r.bytesDone + min(r.cur, r.curMax)
	s := Snapshot{State: r.state, Done: r.doneN, Total: r.total, Failed: r.failedN, BytesDone: done,
		BytesTotal: r.bytesTotal, Current: r.current, Err: r.err, ErrAt: r.errAt,
		Renamed: r.renamedN, Removed: r.removedN, Failures: append([]Failure(nil), r.failures...)}
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
	j := &job{Runner: r, ctx: ctx, p: p, m: m, saved: r.Now()}
	defer func() { // a stop keeps what is done; a crash re-fetches at most the songs since the last save
		if j.unsaved > 0 {
			j.save()
		}
	}()
	for _, phase := range []func() error{j.renames, j.downloads, j.keep, j.playlists, j.removals} {
		if err := phase(); err != nil {
			return err
		}
	}
	return nil
}

// job is one run of a plan over the manifest it loaded, phase by phase. The manifest never names a
// file that isn't there: each phase records a change before the file it frees goes.
type job struct {
	*Runner
	ctx     context.Context
	p       plan.Plan
	m       *library.Manifest
	unsaved int       // changes not on the card yet
	saved   time.Time // when the manifest was last saved
	failed  bool      // a wanted song is missing: nothing is removed this run (I2)
}

// count adds one to a counter the snapshot reads.
func (j *job) count(n *int) {
	j.mu.Lock()
	*n++
	j.mu.Unlock()
}

func (j *job) save() error {
	if !j.CardMounted() { // never MkdirAll .subsonic onto the Q2's own storage under the mount point
		return q2.ErrNoCard
	}
	j.op("save", "")
	if err := j.m.Save(j.root, j.manifestPath()); err != nil {
		return j.cardErr(err)
	}
	j.unsaved, j.saved = 0, j.Now()
	return nil
}

// renames moves songs whose path changed, saving after each (M13 finishes one cut before its save).
func (j *job) renames() error {
	recorded := map[string]bool{} // every song's file, case-folded: a rename never replaces one
	for _, e := range j.m.Songs {
		recorded[strings.ToLower(e.Path)] = true
	}
	for _, mv := range j.p.Rename {
		if err := j.check(j.ctx); err != nil {
			return err
		}
		e := j.m.Songs[mv.ID]
		if e == nil || e.Path == mv.To {
			continue
		}
		if err := j.rename(e.Path, mv.To, !recorded[strings.ToLower(mv.To)]); err != nil {
			if !j.CardMounted() {
				return q2.ErrNoCard
			}
			j.fail(mv.ID, filepath.Base(mv.To), "rename", err)
			continue
		}
		delete(recorded, strings.ToLower(e.Path))
		recorded[strings.ToLower(mv.To)] = true
		e.Path = mv.To // its MetaKey with the rest of p.Keep, after the downloads
		j.count(&j.renamedN)
		if err := j.save(); err != nil {
			return err
		}
	}
	return nil
}

// downloads fetches the adds and replaces, recording them in batches (M18). It ends the run on a
// failure the whole sync shares (I3, R39).
func (j *job) downloads() error {
	covers := map[string]bool{} // album folders whose cover was tried this run
	streak := 0                 // songs failed in a row for the same reason as the whole sync would (I3)
	for _, w := range append(append([]plan.Want(nil), j.p.Add...), j.p.Replace...) {
		id := w.Song.ID
		if e := j.m.Songs[id]; e != nil && e.Path == w.Path && e.Quality == w.Quality && e.Meta == library.MetaKey(w.Song, w.Quality) {
			j.finished(w)
			continue
		}
		if err := j.check(j.ctx); err != nil {
			return err
		}
		j.mu.Lock()
		j.current = w.Song.Title
		if w.Song.Artist != "" {
			j.current = w.Song.Artist + " – " + w.Song.Title
		}
		j.mu.Unlock()
		n, err := j.fetchTries(j.ctx, w)
		if err != nil {
			if j.ctx.Err() != nil {
				return j.ctx.Err()
			}
			if !j.CardMounted() {
				return q2.ErrNoCard
			}
			j.fail(id, w.Song.Title, "download", err)
			j.failed = true
			switch stop := stopReason(err); {
			case errors.Is(stop, ErrRefused): // the server's answer for the account: at once
				return stop
			case stop == nil:
				streak = 0
			default: // a connection or the card can come back: two songs in a row
				if streak++; streak >= 2 {
					return stop
				}
			}
			continue
		}
		streak = 0
		old := j.m.Songs[id]
		e := &library.Entry{ID: id, Path: w.Path, Quality: w.Quality, Size: n, Items: w.Items,
			Meta: library.MetaKey(w.Song, w.Quality)}
		if w.Quality == "original" {
			e.SrcSize = w.Song.Size
		}
		j.m.Songs[id] = e
		j.unsaved++
		switch {
		case old != nil && !oneFile(old.Path, w.Path):
			if err := j.save(); err != nil { // the new file recorded before the old one goes
				return err
			}
			j.remove(old.Path)
		case j.unsaved >= saveEvery || j.Now().Sub(j.saved) >= saveAfter:
			if err := j.save(); err != nil {
				return err
			}
		}
		if dir := filepath.Dir(w.Path); !covers[dir] {
			covers[dir] = true
			j.cover(j.ctx, w)
		}
		j.finished(w)
		j.op("download", w.Path)
	}
	return nil
}

// keep records the items and MetaKeys of songs that needed no transfer (I12).
func (j *job) keep() error {
	if applyKeep(j.m, j.p.Keep) {
		j.unsaved++
	}
	return nil
}

// playlists writes every selected playlist's .m3u, naming where each song is, and deletes the
// recorded ones no longer selected (unless a download failed).
func (j *job) playlists() error {
	m, p := j.m, j.p
	for name, f := range m.Playlists {
		switch {
		case !library.ValidPlaylistFile(j.root, f):
			j.logf("sync: ignoring bad recorded playlist path %q for %s", f, name)
			delete(m.Playlists, name)
		case library.StalePlaylistFile(f): // an older build's name, which the Q2 refuses or cuts
			if err := m.Remove(j.root, f); err != nil {
				j.logf("sync: remove stale playlist %s: %v", f, err) // kept, and tried again next sync
			} else {
				delete(m.Playlists, name) // written again below under a name that fits, and sent as "add" (R35)
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
			m.Playlists[name] = j.freePlaylistFile(m, name, false)
			j.unsaved++
		}
	}
	if j.unsaved > 0 {
		if err := j.save(); err != nil {
			return err
		}
	}
	for _, name := range names {
		if err := j.check(j.ctx); err != nil { // USB storage gives the app 3 s to let the card go
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
		if err := m.WriteM3U(j.root, f, have); err != nil {
			return j.cardErr(err)
		}
		m.PlaylistSums[name] = library.PlaylistSum(p.Playlists[name]) // what the next plan compares (I10)
		j.unsaved++                                                   // saved with a stop between playlists too
		j.op("playlist", f)
		if migrating {
			library.Remove(j.root, library.LegacyPlaylistFile(j.root, name))
		}
	}
	if !j.failed {
		for name, f := range m.Playlists {
			if _, ok := p.Playlists[name]; !ok {
				if m.Remove(j.root, f) == nil {
					delete(m.Playlists, name)
					delete(m.PlaylistSums, name)
				}
			}
		}
	}
	if migrating {
		legacy, _ := filepath.Glob(filepath.Join(j.root, "_explaylist_data", "Navidrome - *.m3u"))
		for _, f := range legacy {
			library.Remove(j.root, f)
		}
	}
	m.PlaylistsStale = false
	return j.save()
}

// removals deletes the songs no longer selected, last and only once every download arrived (I2):
// each batch leaves the manifest before its files go (I4).
func (j *job) removals() error {
	if j.failed {
		return nil // I2: the card never ends with less than before; Retry Failed or the next Sync removes
	}
	m := j.m
	keep := map[string]bool{} // files other songs stay in (one file under two names, from older builds)
	gone := map[string]bool{}
	for _, e := range j.p.Remove {
		gone[e.ID] = true
	}
	for id, e := range m.Songs {
		if !gone[id] {
			keep[strings.ToLower(e.Path)] = true
		}
	}
	for rest := j.p.Remove; len(rest) > 0; {
		if err := j.check(j.ctx); err != nil {
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
		j.unsaved++
		if err := j.save(); err != nil {
			return err
		}
		for _, e := range es {
			if keep[strings.ToLower(e.Path)] {
				continue
			}
			if err := j.remove(e.Path); err != nil {
				j.logf("sync: remove %s: %v", e.ID, err)
				m.Songs[e.ID] = e // still there: the next sync tries again
				j.unsaved++
				continue
			}
			j.count(&j.removedN)
			j.op("remove", e.Path)
		}
	}
	return j.save()
}

const (
	saveEvery   = 20               // downloads recorded per manifest save (M18) ...
	saveAfter   = 30 * time.Second // ... or this long since the last
	removeBatch = 100              // removals dropped from the manifest per save
)

// ErrOffline, ErrCardFull and ErrRefused pause a sync: every song would fail the same way.
var (
	ErrOffline  = fmt.Errorf("lost the server: %w", subsonic.ErrUnreachable)
	ErrCardFull = errors.New("the card is full")
	ErrRefused  = errors.New("the server refused this account") // wraps the server's error: its code says why
)

// stopReason tells a failure the whole sync shares from one of this song's own (an HTTP error, or
// a Subsonic one such as 70, not found): ErrOffline (no answer, or a proxy's answer that the
// server is down, wrapping err), ErrCardFull, or ErrRefused (wrapping err) for an answer about the
// account: the sign-in (40-44) or downloads turned off for it (50, R39). nil for the song's own.
func stopReason(err error) error {
	var ae *subsonic.APIError
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ENOSPC):
		return ErrCardFull
	case errors.Is(err, subsonic.ErrAuth) || errors.As(err, &ae) && ae.Code >= 40 && ae.Code <= 50:
		return fmt.Errorf("%w: %w", ErrRefused, err)
	case errors.As(err, &ae):
		if ae.HTTP == 0 || serverDown(ae.HTTP) {
			return fmt.Errorf("%w: %w", ErrOffline, err)
		}
		return nil
	case errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF):
		return ErrOffline
	}
	return nil
}

// serverDown reports an HTTP status a proxy or CDN in front of the server answers for every
// request while the server is down or overloaded (Caddy's 502, Cloudflare's 520-527, 503, 504), or
// the server's own 429 (too many transcodes).
func serverDown(status int) bool {
	return status == 429 || status >= 502 && status <= 504 || status >= 520 && status <= 527
}

// freePlaylistFile picks <name>.m3u (unless skipBase), else <name> (Subsonic).m3u, else
// <name> (Subsonic 2).m3u... (library.PlaylistFileN): the first neither on the card (a user's own
// file) nor recorded for another playlist (case-folded: the card is FAT/exFAT).
func (r *Runner) freePlaylistFile(m *library.Manifest, name string, skipBase bool) string {
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
	n := 0
	if skipBase {
		n = 1
	}
	f := library.PlaylistFileN(r.root, name, n)
	for taken(f) {
		n++
		f = library.PlaylistFileN(r.root, name, n)
	}
	return f
}

// remove deletes path through the guard and, when its album folder is left holding only
// cover.jpg and a replaced plan's leftover .part and .part.key files, those too, so the guard
// prunes the folder.
// ponytail: a leftover of a song later in this run, paused earlier, is fetched again from the start.
func (r *Runner) remove(path string) error {
	if err := library.Remove(r.root, path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range es {
		if n := e.Name(); e.IsDir() || n != "cover.jpg" && !strings.HasSuffix(n, ".part") && !strings.HasSuffix(n, ".part.key") {
			return nil // a song, or something else, still lives here
		}
	}
	for _, e := range es {
		library.Remove(r.root, filepath.Join(dir, e.Name()))
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

// rename moves a song's file from from to to. A file already at to is replaced only when orphan
// (no song records it: one the app forgot in a crash window), else the move fails.
func (r *Runner) rename(from, to string, orphan bool) error {
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
	case terr == nil && !orphan:
		return errors.New("rename target exists: " + to)
	case terr == nil:
		r.logf("sync: replacing %s, which no song records", to)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		return err
	}
	r.carryCover(filepath.Dir(from), filepath.Dir(to))
	return r.remove(from) // the file is gone already; this prunes the folders it left empty
}

// carryCover copies the album's cover.jpg into a song's new folder when that has none: the old
// one goes with the old folder's last song, and a renamed song is never downloaded (M17).
func (r *Runner) carryCover(from, to string) {
	src, dst := filepath.Join(from, "cover.jpg"), filepath.Join(to, "cover.jpg")
	if _, err := os.Stat(dst); err == nil || owned(r.root, dst) != nil {
		return
	}
	if b, err := os.ReadFile(src); err == nil {
		q2.WriteFile(dst, b)
	}
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
		if ctx.Err() != nil || !r.CardMounted() || errors.Is(stopReason(err), ErrRefused) {
			return 0, err // a refusal for the account is the same on every try
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
	limit := Limit(resp.ContentLength, w.Bytes)
	n, err := r.copy(f, io.LimitReader(resp.Body, limit+1), from, yield)
	if err == nil {
		err = SizeErr(n-from, resp.ContentLength, limit) // checked before the rename (I8)
	}
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

// Limit is the most an answer's body may hold: its Content-Length when the server sent one, else
// twice the size expected (want, an estimate for a transcode) and 8 MiB, so a server that never
// stops can't fill the card (M9); no limit when neither is known.
func Limit(contentLength, want int64) int64 {
	switch {
	case contentLength >= 0:
		return contentLength
	case want > 0:
		return 2*want + 8<<20
	}
	return 1<<63 - 2
}

// SizeErr refuses a body of n bytes that is empty, short of its Content-Length (when one came) or
// past limit: it is no song (I8, M9). A server's own Size isn't the measure: a transcode differs.
func SizeErr(n, contentLength, limit int64) error {
	switch {
	case n == 0:
		return errors.New("the server sent an empty file")
	case contentLength >= 0 && n < contentLength:
		return fmt.Errorf("the server sent %d of %d bytes", n, contentLength)
	case n > limit:
		return fmt.Errorf("the server sent more than the %d bytes expected", limit)
	}
	return nil
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
		resp, err := r.src.Get(ctx, sized(r.src.CoverURL(w.Song.CoverArt)), 0)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxCover+1))
		if err != nil {
			return err
		}
		if len(b) > maxCover {
			return errors.New("cover larger than 16 MiB")
		}
		return q2.WriteFile(c, b)
	}()
	if err != nil && ctx.Err() == nil {
		r.fail(w.Song.ID, w.Song.Title, "cover", err)
	}
}

const maxCover = 16 << 20

// sized asks getCoverArt for 600 px: the original can be many megabytes (M19), which the plan's
// space estimate doesn't count.
// ponytail: here, as subsonic.Client.CoverURL takes no size.
func sized(u string) string {
	p, err := url.Parse(u)
	if err != nil || u == "" {
		return u
	}
	q := p.Query()
	q.Set("size", "600")
	p.RawQuery = q.Encode()
	return p.String()
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
