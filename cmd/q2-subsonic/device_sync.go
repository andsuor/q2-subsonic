package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andsuor/q2-subsonic/internal/fw"
	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/plan"
	"github.com/andsuor/q2-subsonic/internal/q2"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/selection"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
	"github.com/andsuor/q2-subsonic/internal/syncer"
)

const planMargin = 16 << 20 // plan.Compute's headroom on top of what a sync needs

// syncPage is the Sync screen: the running sync, the one just finished, or the preview, computed
// in the background while the page refreshes.
func (d *daemon) syncPage(cl *subsonic.Client) q2spot.Reply {
	snap := d.runner.Snapshot()
	d.mu.Lock()
	showDone := d.showDone
	d.mu.Unlock()
	switch {
	case snap.State == "running" || snap.State == "paused":
		return d.syncRows(snap)
	case snap.State == "done" && showDone:
		return d.doneRows(snap)
	case snap.State == "error" && showDone:
		msg := "Sync stopped"
		if snap.Err != nil {
			msg += " · " + subsonic.Message(snap.Err, d.hostName())
		}
		return q2spot.Reply{Rows: []q2spot.Row{info(msg), act("retry", "Retry", ""), act("check", "Check Again", "")}}
	}
	checking := q2spot.Reply{Message: "refresh", Rows: []q2spot.Row{info("Checking Navidrome…")}}
	d.mu.Lock()
	p := d.preview
	if p == nil || p.gen != d.gen || p.done && time.Since(p.at) > previewTTL {
		p = &preview{gen: d.gen, at: time.Now()}
		d.preview = p
		d.mu.Unlock()
		go d.computePreview(cl, p)
		return checking
	}
	done, pl, err := p.done, p.plan, p.err
	if done && err != nil {
		d.preview = nil // shown once; the next Sync tries again
	}
	d.mu.Unlock()
	switch {
	case !done:
		return checking
	case err != nil:
		return d.errRows(err, "")
	}
	return previewRows(pl)
}

// computePreview resolves every selected item to its songs, 4 at a time, and plans the sync. Any
// failure fails the preview: a missing item's songs would otherwise read as removed.
func (d *daemon) computePreview(cl *subsonic.Client, p *preview) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	sel, err := d.selection()
	resolved, names := map[string][]subsonic.Song{}, map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	var werr error // the first worker's failure, under mu; it cancels ctx, so the rest return at once
	items := sel.Items
	for _, it := range items {
		if err != nil {
			break // the selection didn't load; no worker has started
		}
		wg.Add(1)
		go func(it selection.Item) {
			defer wg.Done()
			if it.Kind == selection.KindLibrary {
				// Holds no slot itself: its requests take them, so 4 stay the most in flight.
				lib, e := d.resolveLibrary(ctx, cl, sem)
				mu.Lock()
				defer mu.Unlock()
				if e != nil && werr == nil {
					werr = e
					cancel()
				}
				key := selection.Key(it.Kind, it.ID)
				resolved[key] = lib.songs
				for _, p := range lib.playlists {
					pk := selection.Key(selection.KindPlaylist, p.id)
					if sel.Has(selection.KindPlaylist, p.id) {
						continue // chosen on its own: that item's quality and songs stand
					}
					sel.Items = append(sel.Items, selection.Item{Kind: selection.KindPlaylist, ID: p.id, Name: p.name, Quality: it.Quality})
					resolved[pk], names[pk] = p.songs, p.name
				}
				return
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			songs, name, e := d.songsOf(ctx, cl, it.Kind, it.ID)
			key := selection.Key(it.Kind, it.ID)
			mu.Lock()
			defer mu.Unlock()
			if e != nil && werr == nil {
				werr = e
				cancel()
			}
			resolved[key], names[key] = songs, name
		}(it)
	}
	wg.Wait()
	if err == nil {
		err = werr
	}
	var pl plan.Plan
	if err == nil {
		pl = plan.Compute(sel, resolved, names, d.manifest(), d.card, d.free(d.card))
	}
	d.mu.Lock()
	p.done, p.plan, p.err, p.at = true, &pl, err, time.Now()
	d.mu.Unlock()
}

func previewRows(p *plan.Plan) q2spot.Reply {
	if len(p.Add)+len(p.Replace)+len(p.Rename)+len(p.Remove) == 0 {
		return q2spot.Reply{Rows: []q2spot.Row{info("Everything is up to date")}}
	}
	var rows []q2spot.Row
	if len(p.Add) > 0 {
		var b int64
		for _, w := range p.Add {
			b += w.Bytes
		}
		rows = append(rows, info("+"+plural(len(p.Add), "song")+" · "+size(b)))
	}
	if len(p.Remove) > 0 {
		var b int64
		for _, e := range p.Remove {
			b += e.Size
		}
		rows = append(rows, list("removed", "−"+plural(len(p.Remove), "song")+" · "+size(b), "", ""))
	}
	if len(p.Replace) > 0 {
		rows = append(rows, info("Replace "+plural(len(p.Replace), "song")))
	}
	if len(p.Rename) > 0 {
		rows = append(rows, info("Rename "+plural(len(p.Rename), "song")))
	}
	rows = append(rows, info("Needs "+size(max(p.NeedBytes, 0))+" · "+size(p.FreeBytes)+" free"))
	if p.Fits {
		rows = append(rows, act("start", "Start Sync", ""))
	} else {
		rows = append(rows, info("Not enough space · Free "+size(p.NeedBytes+planMargin-p.FreeBytes)))
	}
	return q2spot.Reply{Rows: rows}
}

// syncRows is the running (or paused) sync, live.
func (d *daemon) syncRows(s syncer.Snapshot) q2spot.Reply {
	pct := fmt.Sprintf("%d %%", s.Permille/10)
	var rows []q2spot.Row
	if d.caps.Progress {
		sub := ""
		if s.State == "paused" {
			sub = "Paused"
		}
		rows = append(rows, q2spot.Row{Kind: "progress", Title: pct, Subtitle: sub, Position: strconv.Itoa(s.Permille)})
	} else {
		n := min(max((s.Permille+50)/100, 0), 10)
		rows = append(rows, info("["+strings.Repeat("#", n)+strings.Repeat("-", 10-n)+"] "+pct))
	}
	rows = append(rows, info(fmt.Sprintf("%d of %s", s.Done, plural(s.Total, "song"))))
	if s.ETA > 0 && s.State == "running" {
		rows = append(rows, info(eta(s.ETA)))
	}
	if s.Current != "" {
		rows = append(rows, info("Now: "+s.Current))
	}
	if s.State == "paused" {
		if errors.Is(s.Err, q2.ErrNoCard) {
			rows = append(rows, info("Card not available"))
		}
		rows = append(rows, act("resume", "Resume", ""))
	} else {
		rows = append(rows, act("pause", "Pause", ""))
	}
	rows = append(rows, act("cancel", "Cancel", ""))
	return q2spot.Reply{Message: "refresh", Rows: rows}
}

func eta(t time.Duration) string {
	m := int(t.Round(time.Minute) / time.Minute)
	switch {
	case m < 1:
		return "Less than a minute left"
	case m < 60:
		return fmt.Sprintf("About %d min left", m)
	}
	return fmt.Sprintf("About %d h %d min left", m/60, m%60)
}

func (d *daemon) doneRows(s syncer.Snapshot) q2spot.Reply {
	rows := []q2spot.Row{info(fmt.Sprintf("Sync complete · %s · %d failed", plural(s.Done, "song"), s.Failed))}
	if s.Failed > 0 {
		rows = append(rows, list("failed", "Failed Songs", "", ""))
	}
	d.mu.Lock()
	lib := d.lib
	updated := lib != nil && lib.done && lib.ok
	d.mu.Unlock()
	if updated {
		rows = append(rows, info("Library updated"))
	} else {
		rows = append(rows, list("library", "Update Your Library", "", ""))
	}
	return q2spot.Reply{Rows: append(rows, act("check", "Check Again", ""))}
}

func (d *daemon) failed() q2spot.Reply {
	rows := []q2spot.Row{act("retry", "Retry Failed", "")}
	for _, f := range d.runner.Snapshot().Failures {
		if f.Op != "cover" {
			rows = append(rows, list("details", f.Title, f.Msg, detailsCtx(f.Op, f.HTTP, f.Code, f.At, f.SongID)))
		}
	}
	return q2spot.Reply{Rows: rows}
}

// removed lists what the previewed sync deletes.
func (d *daemon) removed(cursor string) q2spot.Reply {
	d.mu.Lock()
	p := d.preview
	ready := p != nil && p.done && p.plan != nil
	d.mu.Unlock()
	var rows []q2spot.Row
	if ready {
		for _, e := range p.plan.Remove {
			rows = append(rows, info(strings.TrimPrefix(e.Path, filepath.Join(d.card, "Navidrome")+"/")))
		}
	}
	if len(rows) == 0 {
		rows = []q2spot.Row{info("Nothing to remove")}
	}
	return page(rows, cursor)
}

func (d *daemon) syncAction(op string) q2spot.Reply {
	switch op {
	case "start":
		d.mu.Lock()
		p := d.preview
		ready := p != nil && p.done && p.err == nil && p.gen == d.gen
		d.mu.Unlock()
		if !ready {
			return q2spot.Reply{Message: "Open Sync again to check"}
		}
		if !p.plan.Fits {
			return q2spot.Reply{Message: "Not enough space"}
		}
		if err := d.runner.Start(*p.plan); err != nil {
			d.log.Printf("sync: start: %v", err)
			return q2spot.Fail(q2spot.Error, "Couldn't start the sync")
		}
		d.mu.Lock()
		d.showDone, d.preview, d.lib = true, nil, nil
		d.mu.Unlock()
	case "pause": // R27: stopping waits for the download to notice; the reply doesn't
		go d.runner.Pause()
	case "resume":
		d.runner.Resume()
	case "cancel":
		go d.runner.Cancel()
	case "retry":
		d.runner.RetryFailed()
		d.mu.Lock()
		d.showDone = true
		d.mu.Unlock()
	case "check":
		d.mu.Lock()
		d.showDone, d.preview = false, nil
		d.mu.Unlock()
	}
	return q2spot.Reply{}
}

// library asks the firmware (F3) to update the library and import the playlists, or says how.
func (d *daemon) library() q2spot.Reply {
	if !d.caps.Library {
		return q2spot.Reply{Rows: []q2spot.Row{info("1. Library → Update Local Music"), info("Run after the sync has finished"),
			info("2. Library → Playlists → Import playlist"), info("Import playlist only applies to synced playlists")}}
	}
	req := filepath.Join(d.tmp, "q2-subsonic.library")
	d.mu.Lock()
	j := d.lib
	if j == nil || j.done && time.Since(j.doneAt) > 30*time.Second {
		seq := time.Now().UnixNano() // never reused, even across restarts
		var m3us []string            // the files the last sync recorded; the user's own playlists are never imported
		if m, _ := library.LoadManifest(filepath.Join(d.card, ".subsonic", "manifest.json")); m != nil {
			for _, f := range m.Playlists {
				if _, err := os.Stat(f); err == nil && library.ValidPlaylistFile(d.card, f) {
					m3us = append(m3us, f)
				}
			}
			sort.Strings(m3us)
		}
		if err := fw.RequestLibrary(req, seq, m3us); err != nil {
			d.mu.Unlock()
			d.log.Printf("library: %v", err)
			return q2spot.Reply{Rows: []q2spot.Row{info("Couldn't ask for a library update"),
				list("details", "Details", "", detailsCtx("library", 0, 0, time.Now(), ""))}}
		}
		j = &libJob{seq: seq}
		d.lib = j
	}
	if !j.done {
		if done, ok, msg := fw.LibraryDone(req+".done", j.seq); done {
			j.done, j.ok, j.msg, j.doneAt = true, ok, msg, time.Now()
		}
	}
	done, ok, msg, at := j.done, j.ok, j.msg, j.doneAt
	d.mu.Unlock()
	switch {
	case !done:
		rows := []q2spot.Row{info("Updating Library…")}
		if d.stream.active() { // ponytail: only our own stream is known; the firmware also waits for local playback
			rows = append(rows, info("Waiting for playback to pause…"))
		}
		return q2spot.Reply{Message: "refresh", Rows: rows}
	case ok:
		return q2spot.Reply{Rows: []q2spot.Row{info("Library updated")}}
	}
	if msg == "" {
		msg = "Library update failed"
	}
	return q2spot.Reply{Rows: []q2spot.Row{info(msg), list("details", "Details", "", detailsCtx("library", 0, 0, at, ""))}}
}

type libPlaylist struct {
	id, name string
	songs    []subsonic.Song
}

type libResult struct {
	songs     []subsonic.Song
	playlists []libPlaylist
}

// resolveLibrary lists every album (500 a page) and every playlist, then fetches each one's songs.
// Every request takes a slot of sem, so the library never runs more than the preview's 4 at once.
// Albums keep their listing order; so do playlists.
func (d *daemon) resolveLibrary(ctx context.Context, cl *subsonic.Client, sem chan struct{}) (libResult, error) {
	ctx, cancel := context.WithCancel(ctx) // the first failure stops the queued fetches
	defer cancel()
	slot := func() func() { sem <- struct{}{}; return func() { <-sem } }
	var albums []subsonic.Album
	for off := 0; ; {
		release := slot()
		page, err := cl.Albums(ctx, off, 500)
		release()
		if err != nil {
			return libResult{}, err
		}
		albums = append(albums, page...)
		if len(page) < 500 {
			break
		}
		off += len(page)
	}
	release := slot()
	pls, err := cl.Playlists(ctx)
	release()
	if err != nil {
		return libResult{}, err
	}
	res := libResult{playlists: make([]libPlaylist, len(pls))}
	albumSongs := make([][]subsonic.Song, len(albums))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	run := func(f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer slot()()
			if ctx.Err() != nil {
				return
			}
			if e := f(); e != nil {
				mu.Lock()
				first = cmp.Or(first, e)
				mu.Unlock()
				cancel()
			}
		}()
	}
	for i, a := range albums {
		run(func() (err error) { _, albumSongs[i], err = cl.Album(ctx, a.ID); return })
	}
	for i, p := range pls {
		run(func() (err error) {
			var pl subsonic.Playlist
			pl, res.playlists[i].songs, err = cl.Playlist(ctx, p.ID)
			res.playlists[i].id, res.playlists[i].name = p.ID, cmp.Or(pl.Name, p.Name)
			return
		})
	}
	wg.Wait()
	if first != nil {
		return libResult{}, first
	}
	res.songs = []subsonic.Song{}
	for _, s := range albumSongs {
		res.songs = append(res.songs, s...)
	}
	d.mu.Lock()
	d.resolved[selection.Key(selection.KindLibrary, "all")] = res.songs
	d.libCount = [2]int{len(albums), len(pls)}
	d.mu.Unlock()
	return res, nil
}
