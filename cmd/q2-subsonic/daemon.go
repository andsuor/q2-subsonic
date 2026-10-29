package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/andsuor/q2-subsonic/internal/applog"
	"github.com/andsuor/q2-subsonic/internal/creds"
	"github.com/andsuor/q2-subsonic/internal/fw"
	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/plan"
	"github.com/andsuor/q2-subsonic/internal/q2"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/selection"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
	"github.com/andsuor/q2-subsonic/internal/syncer"
	"github.com/andsuor/q2-subsonic/internal/tlsroots"
)

const (
	answerIn    = 8 * time.Second  // every Q2SPOT/1 reply within this (the firmware gives up at 15 s)
	idleAfter   = 30 * time.Minute // exit 0 after this long with no request, sync or stream
	signinPort  = 8080             // or the next free one
	previewTTL  = time.Minute      // a preview older than this is computed again when Sync is opened
	pingEvery   = 30 * time.Second
	qrLeftAfter = 5 * time.Second // a gap this long in the signed-out menu's 2 s refresh: the QR page was open
)

var errSignedOut = errors.New("signed out")

// settings is .subsonic/settings.json. StreamQuality "" follows DefaultQuality.
type settings struct {
	DefaultQuality string `json:"defaultQuality,omitempty"`
	StreamQuality  string `json:"streamQuality,omitempty"`
}

func (s settings) def() string {
	if s.DefaultQuality == "" {
		return "original"
	}
	return s.DefaultQuality
}

func (s settings) stream() string {
	if s.StreamQuality == "" {
		return s.def()
	}
	return s.StreamQuality
}

// stateKeys is the state file: sign-in's QR keys, hold=1 and version, merged so neither writer
// drops the other's keys.
type stateKeys struct {
	st   *q2.State
	mu   sync.Mutex
	sign map[string]string
	hold bool
}

func (s *stateKeys) Write(kv map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sign = kv
	return s.flush()
}

func (s *stateKeys) setHold(h bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold = h
	return s.flush()
}

func (s *stateKeys) remove() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Remove()
}

func (s *stateKeys) flush() error {
	m := map[string]string{"version": version}
	for k, v := range s.sign {
		m[k] = v
	}
	if s.hold {
		m["hold"] = "1"
	}
	return s.st.Write(m)
}

type preview struct {
	gen  int
	at   time.Time
	done bool
	plan *plan.Plan
	err  error
}

type libJob struct {
	seq    int64
	done   bool
	ok     bool
	msg    string
	doneAt time.Time
}

// daemon is the running app: the Q2SPOT/1 handler (device) and what it drives.
type daemon struct {
	dir, card, tmp string // the app folder (.subsonic), the card's root, /tmp
	caps           fw.Caps
	log            *applog.Log
	state          *stateKeys
	signin         *signin
	runner         *syncer.Runner
	stream         *streamer
	newClient      func(subsonic.Creds) (*subsonic.Client, error)
	free           func(path string) int64
	answerIn       time.Duration
	signinPort     int

	mu                 sync.Mutex
	cl                 *subsonic.Client
	server, user, host string
	lastReq, lastMenu  time.Time
	pingAt             time.Time
	pingErr            error
	pinging, pinged    bool
	gen                int // bumped by every selection change
	preview            *preview
	showDone           bool // the finished sync's page, until the selection changes or Check Again
	lib                *libJob
	resolved           map[string][]subsonic.Song // selection key -> its songs, as last fetched
	libCount           [2]int                     // albums, playlists of the Entire Library, as last resolved
	playSeq            int                        // the latest play request; an older one still waiting is dropped
	playMu             sync.Mutex                 // one streamer.start at a time
	man                *library.Manifest
	manAt              time.Time
	manSize            int64
	migrateErr         error // the one-time Navidrome/ -> Subsonic/ move failed; Sync shows it
}

func newDaemon(dir, card, tmp string) *daemon {
	os.MkdirAll(dir, 0o755)
	lg := applog.Open(filepath.Join(dir, "log.txt"), 1<<20)
	d := &daemon{dir: dir, card: card, tmp: tmp, log: lg,
		caps:  fw.ReadCaps(filepath.Join(tmp, "q2-addon.caps")),
		state: &stateKeys{st: &q2.State{Path: filepath.Join(tmp, "q2-subsonic.state")}},
		newClient: func(c subsonic.Creds) (*subsonic.Client, error) {
			return subsonic.New(c, tlsroots.Client(15*time.Second))
		},
		free: freeBytes, answerIn: answerIn, signinPort: signinPort,
		lastReq: time.Now(), resolved: map[string][]subsonic.Song{}}
	d.signin = &signin{dir: dir, state: d.state, newClient: func(c subsonic.Creds) (*subsonic.Client, error) {
		cl, err := d.newClient(c)
		if cl != nil {
			cl.Log = lg // the sign-in ping goes in log.txt too, redacted
		}
		return cl, err
	}}
	d.runner = syncer.New(card, src{d}, lg)
	d.runner.OnDone = d.syncDone
	d.stream = newStreamer(card, filepath.Join(tmp, "q2-subsonic.play"), filepath.Join(tmp, "q2-subsonic.playing"),
		d.caps, src{d}, d.manifest, syncer.Net, time.Second)
	d.stream.log = lg
	d.stream.quality = func() string { return d.settings().stream() }
	d.state.Write(nil)
	if err := library.Migrate(card, lg.Printf); err != nil {
		d.migrateErr = err
	}
	return d
}

func freeBytes(path string) int64 {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// client is the signed-in server client, loaded from the card's credentials; nil when signed out.
func (d *daemon) client() *subsonic.Client {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cl != nil {
		return d.cl
	}
	c, ok := creds.Load(d.dir)
	if !ok {
		return nil
	}
	cl, err := d.newClient(c)
	if err != nil {
		return nil
	}
	cl.Log = d.log
	d.cl, d.server, d.user, d.host = cl, c.Server, c.User, c.Server
	if u, err := url.Parse(c.Server); err == nil && u.Hostname() != "" {
		d.host = u.Hostname()
	}
	tlsroots.SetDateURL(c.Server)
	return cl
}

func (d *daemon) hostName() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.host
}

// signOut forgets the sign-in at once; the sync and stream stop in the background (R27: a reply
// never waits for a download to notice its cancel).
func (d *daemon) signOut() {
	creds.Remove(d.dir)
	d.mu.Lock()
	d.cl, d.pinged, d.preview = nil, false, nil
	d.playSeq++ // a play still waiting for playMu is dropped
	d.mu.Unlock()
	go d.runner.Pause()
	go func() { // under playMu, so a start already running finishes first and is then stopped
		d.playMu.Lock()
		defer d.playMu.Unlock()
		d.stream.stop()
	}()
}

// src lends the current client to the runner and the streamer, which outlive a sign-in.
type src struct{ d *daemon }

func (s src) MediaURL(id, q string) string {
	if c := s.d.client(); c != nil {
		return c.MediaURL(id, q)
	}
	return ""
}

func (s src) CoverURL(id string) string {
	if c := s.d.client(); c != nil {
		return c.CoverURL(id)
	}
	return ""
}

func (s src) Get(ctx context.Context, u string, from int64) (*http.Response, error) {
	if c := s.d.client(); c != nil {
		return c.Get(ctx, u, from)
	}
	return nil, errSignedOut
}

// manifest is the card's manifest, read again only when the file changed.
func (d *daemon) manifest() *library.Manifest {
	p := filepath.Join(d.card, ".subsonic", "manifest.json")
	st, err := os.Stat(p)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.man == nil || err != nil || !st.ModTime().Equal(d.manAt) || st.Size() != d.manSize {
		d.man, _ = library.LoadManifest(p)
		d.manAt, d.manSize = time.Time{}, 0
		if err == nil {
			d.manAt, d.manSize = st.ModTime(), st.Size()
		}
	}
	return d.man
}

func (d *daemon) selPath() string { return filepath.Join(d.dir, "selection.json") }

func (d *daemon) selection() (selection.Set, error) { return selection.Load(d.selPath()) }

func (d *daemon) saveSelection(s selection.Set) error {
	err := s.Save(d.selPath())
	d.mu.Lock()
	d.gen++
	d.showDone = false
	d.mu.Unlock()
	return err
}

func (d *daemon) settings() settings {
	var s settings
	if b, err := os.ReadFile(filepath.Join(d.dir, "settings.json")); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

func (d *daemon) saveSettings(s settings) error {
	b, _ := json.Marshal(s)
	return q2.WriteFile(filepath.Join(d.dir, "settings.json"), b)
}

// tick keeps the state file's hold=1 while files move, the phone page is open or a stream plays.
func (d *daemon) tick() {
	d.state.setHold(d.runner.Busy() || d.signin.active() || d.stream.active())
}

func (d *daemon) idle(now time.Time) bool {
	d.mu.Lock()
	last := d.lastReq
	d.mu.Unlock()
	return now.Sub(last) >= idleAfter && !d.runner.Busy() && !d.stream.active()
}

// close ends what the app runs before it exits; a sync paused here resumes at the next Sync.
func (d *daemon) close(sock string) {
	d.signin.stop()
	d.stream.stop()
	d.runner.Pause()
	d.state.remove()
	os.Remove(sock)
}

// serve answers Q2SPOT/1 on sock, and exits 0 once idle.
func (d *daemon) serve(sock string) error {
	go q2.Every(time.Second, nil, func() {
		d.tick()
		if d.idle(time.Now()) {
			d.log.Printf("idle, exiting")
			d.close(sock)
			os.Exit(0)
		}
	})
	return q2spot.Serve(sock, d.device)
}
