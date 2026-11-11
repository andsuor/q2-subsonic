package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// reports records what plays sends: "now <id>", or "<id>@<ms>" for a play ("<id>@" with no time).
type reports struct {
	mu   sync.Mutex
	got  []string
	fail func(id string) error // optional: a play's answer
}

func (r *reports) send(_ context.Context, id string, at time.Time, submission bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case !submission:
		r.got = append(r.got, "now "+id)
		return nil
	case at.IsZero():
		r.got = append(r.got, id+"@")
	default:
		r.got = append(r.got, fmt.Sprint(id, "@", at.UnixMilli()))
	}
	if r.fail != nil {
		return r.fail(id)
	}
	return nil
}

func (r *reports) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

// newPlays counts a song after a millisecond a second of the rule; the clock says ms, a step a call.
func newPlays(t *testing.T) (*plays, *reports) {
	r, tick := &reports{}, int64(0)
	p := &plays{send: r.send, on: func() bool { return true }, unit: time.Millisecond,
		clock: func() time.Time { tick += 1000; return time.UnixMilli(tick) }}
	t.Cleanup(func() { p.clear(); p.rest(t) })
	return p, r
}

// rest waits until nothing is armed or on its way.
func (p *plays) rest(t *testing.T) {
	t.Helper()
	until(t, "reports end", func() bool { return !p.pending() })
}

func (r *reports) want(t *testing.T, want ...string) {
	t.Helper()
	until(t, fmt.Sprint(want), func() bool { return reflect.DeepEqual(r.all(), want) })
}

// reset forgets what was sent, and answers plays with fail from now on.
func (r *reports) reset(fail func(id string) error) {
	r.mu.Lock()
	r.got, r.fail = nil, fail
	r.mu.Unlock()
}

func song(id string, secs int) subsonic.Song { return subsonic.Song{ID: id, Duration: secs} }

var errAway = fmt.Errorf("away: %w", subsonic.ErrUnreachable)

func away(string) error { return errAway }

func TestPlaysNowThenPlayed(t *testing.T) {
	p, r := newPlays(t)
	p.now(song("a", 400), 0) // counts after 200 ms
	r.want(t, "now a")
	if !p.pending() {
		t.Fatal("not pending")
	}
	r.want(t, "now a", "a@1000")
	p.rest(t)
}

// A song that starts before the one playing counted takes its place; one of 30 s never counts;
// and a list left takes no play with it.
func TestPlaysNextSongCancels(t *testing.T) {
	p, r := newPlays(t)
	p.now(song("a", 100), 0)
	p.now(song("b", 30), 0)
	p.mu.Lock()
	armed := p.timer != nil
	p.mu.Unlock()
	if armed {
		t.Fatal("a 30 s song is waited for")
	}
	p.now(song("c", 100), 0)
	p.cancel()
	time.Sleep(120 * time.Millisecond)
	p.rest(t)
	for _, g := range r.all() {
		if !strings.HasPrefix(g, "now ") {
			t.Fatalf("sent %q", r.all())
		}
	}
}

// A start seen late counts that much sooner, from the time it started.
func TestPlaysLate(t *testing.T) {
	p, r := newPlays(t)
	p.unit = time.Hour
	late := 20*time.Hour - 10*time.Millisecond
	p.now(song("a", 40), late)
	r.want(t, "now a", fmt.Sprint("a@", 1000-late.Milliseconds()))
}

// Plays the server is away for wait, at most 20, and go oldest first with their own times after
// the next one; the first that fails again ends that round.
func TestPlaysQueue(t *testing.T) {
	p, r := newPlays(t)
	r.reset(away)
	for i := 0; i < 22; i++ {
		p.now(song(fmt.Sprint("s", i), 31), 0)
		until(t, "queued", func() bool { return !p.pending() })
	}
	p.mu.Lock()
	n, first := len(p.queue), p.queue[0].id
	p.mu.Unlock()
	if n != playsMax || first != "s2" {
		t.Fatalf("%d queued, from %s", n, first)
	}
	r.reset(func(id string) error {
		if id == "s4" {
			return errAway
		}
		return nil
	})
	p.cancel() // another song pressed: the queue stays
	p.now(song("z", 31), 0)
	r.want(t, "now z", "s3@4000", "s4@5000")
	p.rest(t)
	r.reset(nil)
	p.now(song("y", 31), 0)
	want := []string{"now y"}
	for i := 4; i < 22; i++ {
		want = append(want, fmt.Sprint("s", i, "@", (i+1)*1000))
	}
	r.want(t, append(want, "z@23000", "y@24000")...)
}

// A play the server refuses is dropped; so is one with no time of its own, whatever the failure.
func TestPlaysDropped(t *testing.T) {
	p, r := newPlays(t)
	r.reset(func(string) error { return errSignedOut })
	p.now(song("a", 31), 0)
	r.want(t, "now a", "a@1000")
	p.rest(t)
	r.reset(away)
	p.clock = func() time.Time { return time.Time{} }
	p.now(song("b", 31), 0)
	r.want(t, "now b", "b@")
	p.rest(t)
	if len(p.queue) != 0 {
		t.Fatalf("queue %v", p.queue)
	}
}

func TestPlaysOffAndClear(t *testing.T) {
	p, r := newPlays(t)
	p.on = func() bool { return false }
	p.now(song("a", 31), 0)
	if p.pending() {
		t.Fatal("pending while off")
	}
	p.on = func() bool { return true }
	r.reset(away)
	p.now(song("b", 31), 0)
	r.want(t, "now b", "b@1000")
	p.rest(t)
	p.clear()
	r.reset(nil)
	p.now(song("c", 31), 0)
	r.want(t, "now c", "c@2000")
}
