package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

const (
	playMost    = 240              // a song counts as played after this many seconds, or half its length
	playLeast   = 30               // a song this short or shorter never counts
	playsMax    = 20               // plays kept for a server that is away; the oldest goes first
	playTimeout = 10 * time.Second // one report
)

type play struct {
	id string
	at time.Time // when the song started; zero when the clock wasn't known
}

// plays reports what a list plays to the server (Subsonic's scrobble): a song as playing now when
// it starts, and as played once it has been the one playing for half its length or playMost, the
// firmware's own listen rule (LISTEN_MIN_S and LISTEN_MAX_S, patch/navigation.c). The firmware
// tells only which song plays, so the time is the wall clock's: a song paused, or left for the
// Library, before then still counts. Nothing here touches playback, and a failure shows nowhere.
type plays struct {
	send  func(ctx context.Context, id string, at time.Time, submission bool) error
	on    func() bool      // Settings' Report Plays; nil is off
	clock func() time.Time // the time now, zero when it isn't known
	unit  time.Duration    // a second; shorter in tests

	mu      sync.Mutex
	timer   *time.Timer // runs until the song playing counts
	queue   []*play     // counted and not yet sent, oldest first; in RAM only
	sending int         // reports on their way
	nowMu   sync.Mutex  // one "playing now" at a time
	flushMu sync.Mutex  // one flush at a time, so no play is sent twice
}

// now is a song's start, late ago: the song before it no longer counts unless it already has.
func (p *plays) now(sg subsonic.Song, late time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancelLocked()
	if p.on == nil || !p.on() {
		return
	}
	wait := playMost * p.unit
	if sg.Duration > 0 {
		wait = min(wait, time.Duration(sg.Duration)*p.unit/2)
	}
	if late < 0 || late > wait {
		late = 0 // the clock was set meanwhile, or the start was seen too late to tell
	}
	at := p.clock()
	if !at.IsZero() {
		at = at.Add(-late)
	}
	p.sending++
	go func() { // never tried again: the next song says it anew
		defer p.sent()
		if !p.nowMu.TryLock() {
			return
		}
		defer p.nowMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), playTimeout)
		defer cancel()
		p.send(ctx, sg.ID, time.Time{}, false)
	}()
	if sg.Duration > 0 && sg.Duration <= playLeast {
		return
	}
	var t *time.Timer
	t = time.AfterFunc(wait-late, func() {
		p.mu.Lock()
		if p.timer != t { // cancelled as it fired
			p.mu.Unlock()
			return
		}
		p.timer = nil
		if p.queue = append(p.queue, &play{sg.ID, at}); len(p.queue) > playsMax {
			p.queue = p.queue[1:]
		}
		p.sending++
		p.mu.Unlock()
		defer p.sent()
		p.flush()
	})
	p.timer = t
}

func (p *plays) sent() {
	p.mu.Lock()
	p.sending--
	p.mu.Unlock()
}

// flush sends the queue, oldest first. A play the server took or refused is done with; at the
// first one that doesn't reach the server the rest wait for the next play, unless it has no time
// of its own, which would then be wrong.
func (p *plays) flush() {
	p.flushMu.Lock()
	defer p.flushMu.Unlock()
	for {
		p.mu.Lock()
		if len(p.queue) == 0 {
			p.mu.Unlock()
			return
		}
		pl := p.queue[0]
		p.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), playTimeout)
		err := p.send(ctx, pl.id, pl.at, true)
		cancel()
		if errors.Is(err, subsonic.ErrUnreachable) && !pl.at.IsZero() {
			return
		}
		p.mu.Lock()
		if len(p.queue) > 0 && p.queue[0] == pl { // else cleared, or pushed out, meanwhile
			p.queue = p.queue[1:]
		}
		p.mu.Unlock()
	}
}

// cancel forgets the song playing: another one was pressed. Plays already counted stay.
func (p *plays) cancel() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancelLocked()
}

func (p *plays) cancelLocked() {
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
}

// clear forgets everything not yet sent, so no play goes to another account (sign-out, the app's
// end, Report Plays turned off).
func (p *plays) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancelLocked()
	p.queue = nil
}

// pending is whether a song may still count, or a report is on its way: the app stays for it.
func (p *plays) pending() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.timer != nil || p.sending > 0
}
