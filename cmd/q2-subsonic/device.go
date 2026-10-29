package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andsuor/q2-subsonic/internal/q2"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/selection"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// The Q2's Navidrome page over Q2SPOT/1 (q2-pod patch/addon.c draws it). A list row's URI is the op it
// opens and its Context the argument; an action row sends URI with Context and toasts the reply's
// message; a held Play/Pause sends the row's Position ("toggle") with its Context ("kind:id").
// Item pages take "kind:id" too, since one Context serves both press and hold. A song row's Context
// is "song:<id> <list kind:id> <index>": toggle reads the first field, play the rest.

var qualities = []string{"original", "mp3-320", "mp3-128"}

func qualityName(q string) string {
	switch q {
	case "mp3-320":
		return "MP3 320"
	case "mp3-128":
		return "MP3 128"
	}
	return "Original"
}

func nextQuality(q string) string {
	for i, v := range qualities {
		if v == q {
			return qualities[(i+1)%len(qualities)]
		}
	}
	return qualities[1]
}

func info(title string) q2spot.Row { return q2spot.Row{Kind: "info", Title: title} }
func act(op, title, ctx string) q2spot.Row {
	return q2spot.Row{Kind: "action", URI: op, Title: title, Context: ctx}
}
func list(op, title, sub, ctx string) q2spot.Row {
	return q2spot.Row{Kind: "list", URI: op, Title: title, Subtitle: sub, Context: ctx}
}

func join(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}

func size(b int64) string {
	if b >= 1e9 {
		return fmt.Sprintf("%.1f GB", float64(b)/1e9)
	}
	return fmt.Sprintf("%d MB", (b+5e5)/1e6)
}

// page is rows from cursor (an offset) on, as many as one reply carries (fit), with the cursor of
// the next page.
func page(rows []q2spot.Row, cursor string) q2spot.Reply {
	off, _ := strconv.Atoi(cursor)
	off = max(0, min(off, len(rows)))
	end := off + fit(rows[off:min(off+q2spot.MaxRows, len(rows))])
	r := q2spot.Reply{Rows: rows[off:end]}
	if end < len(rows) {
		r.Cursor = strconv.Itoa(end)
	}
	return r
}

// fit is how many of rows (at most 50) one reply carries with room for a Sync Now row: a row that
// Encode left out for 64 KiB would never be shown, as the cursor would go on past it (M28).
func fit(rows []q2spot.Row) int {
	// The longest message stands in for "pages" and the Sync Now row's bytes.
	r := q2spot.Reply{Message: strings.Repeat("x", q2spot.MaxMessage), Cursor: "9999999999", Rows: rows}
	_, kept := r.Encode("99999999999999999999")
	return min(len(rows), max(kept, 1))
}

// listErr is a list that failed: on its first page the message and Details; on a later one, which
// the firmware adds to the list while it loads it page by page, a failed reply, so the list keeps
// More… to try again (M30).
func (d *daemon) listErr(err error, cursor string) q2spot.Reply {
	if cursor != "" {
		return d.errToast(err)
	}
	return d.errRows(err, "")
}

// parseKey splits "kind:id" (or a song row's Context, whose first field it is).
func parseKey(arg string) (selection.Kind, string) {
	f, _, _ := strings.Cut(arg, " ")
	k, id, _ := strings.Cut(f, ":")
	return selection.Kind(k), id
}

func (d *daemon) device(req q2spot.Request) q2spot.Reply {
	r := d.reply(req)
	// F-R8: the firmware auto-pages a list only when its Message is "pages" and it has a cursor;
	// otherwise it shows a More… row. Refresh levels keep "refresh" and never page.
	if r.Cursor != "" && r.Message == "" {
		r.Message = "pages"
	}
	return r
}

func (d *daemon) reply(req q2spot.Request) q2spot.Reply {
	d.mu.Lock()
	d.lastReq = time.Now()
	d.mu.Unlock()
	switch req.Op {
	case "menu":
		return d.menu()
	case "details":
		return details(req.Arg(0))
	case "settings", "defq", "streamq":
		return d.settingsOp(req.Op)
	case "onq2":
		return d.onQ2(req.Arg(0))
	case "resetsel":
		return d.resetSelection()
	case "migrate":
		return d.migrate()
	case "signin":
		d.signin.start(d.signinPort)
		return q2spot.Reply{Message: "Scan the code with your phone"}
	}
	cl := d.client()
	if cl == nil {
		if req.Op == "account" {
			return q2spot.Reply{Rows: []q2spot.Row{info("Signed out")}}
		}
		return q2spot.Fail(q2spot.Login, "Sign in first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.answerIn*7/8)
	defer cancel()
	switch req.Op {
	case "playlists", "artists", "albums":
		return d.withSyncNow(d.browse(ctx, cl, req.Op, req.Arg(0)), req.Arg(0))
	case "artist", "album", "playlist":
		return d.withSyncNow(d.itemPage(ctx, cl, req.Arg(0), req.Arg(1)), req.Arg(1))
	case "search":
		return d.withSyncNow(d.search(ctx, cl, req.Arg(0), req.Arg(1)), req.Arg(1))
	case "toggle":
		return d.toggle(ctx, cl, req.Arg(0))
	case "quality":
		return d.quality(req.Arg(0))
	case "play":
		return d.play(ctx, cl, req.Arg(0))
	case "account":
		return d.account(cl)
	case "signout":
		d.signOut()
		return q2spot.Reply{Message: "Signed out"}
	case "sync":
		return d.syncPage(cl)
	case "start", "pause", "resume", "cancel", "retry", "check":
		return d.syncAction(req.Op)
	case "removed":
		return d.removed(req.Arg(0))
	case "failed":
		return d.failed(req.Arg(0))
	case "library":
		return d.library()
	}
	return q2spot.Fail(q2spot.Unsupported, "Unknown request")
}

// errRows is a failed list: the message and a Details row. The status stays ok because the
// firmware shows only the message and Retry, never the rows, for any other status.
func (d *daemon) errRows(err error, song string) q2spot.Reply {
	host := d.hostName()
	d.log.Printf("error: %v", err)
	var ae *subsonic.APIError
	op, h, code := "", 0, 0
	if errors.As(err, &ae) {
		op, h, code = ae.Op, ae.HTTP, ae.Code
	}
	return q2spot.Reply{Rows: []q2spot.Row{info(subsonic.Message(err, host)),
		list("details", "Details", "", detailsCtx(op, h, code, time.Now(), song))}}
}

// errToast is a failed action's reply: the firmware toasts its message.
func (d *daemon) errToast(err error) q2spot.Reply {
	host := d.hostName()
	d.log.Printf("error: %v", err)
	st := q2spot.Error
	if errors.Is(err, subsonic.ErrUnreachable) {
		st = q2spot.Offline
	}
	return q2spot.Fail(st, subsonic.Message(err, host))
}

// detailsCtx carries what the Details page shows, so it needs no state: op|http|code|unix|song.
func detailsCtx(op string, h, code int, at time.Time, song string) string {
	return fmt.Sprintf("%s|%d|%d|%d|%s", op, h, code, at.Unix(), song)
}

func details(ctx string) q2spot.Reply {
	f := strings.SplitN(ctx, "|", 5)
	for len(f) < 5 {
		f = append(f, "")
	}
	unix, _ := strconv.ParseInt(f[3], 10, 64)
	rows := []q2spot.Row{info("Op: " + f[0]), info("HTTP: " + f[1]), info("Code: " + f[2]),
		info("Time: " + time.Unix(unix, 0).UTC().Format("2006-01-02 15:04:05 UTC"))}
	if f[4] != "" {
		rows = append(rows, info("Song: "+f[4]))
	}
	return q2spot.Reply{Rows: rows}
}

// menu is the root. Signed out it is the phone sign-in, kept live so the root turns into the
// signed-in menu once the phone succeeds. The firmware's remote row sends nothing, so sign-in runs
// while the root shows; the 2 s refresh pauses while the QR page is on top, and the first request
// after such a gap means the QR page was left: that sign-in stops and a fresh one (new key and PIN) starts.
func (d *daemon) menu() q2spot.Reply {
	now := time.Now()
	d.mu.Lock()
	gap := now.Sub(d.lastMenu)
	d.lastMenu = now
	d.mu.Unlock()
	cl := d.client()
	if cl == nil {
		ok, msg := d.signin.result()
		active, left := d.signin.active(), gap > qrLeftAfter
		switch {
		case active && !ok && left:
			d.signin.stop()
			d.signin.start(d.signinPort)
		case !active && msg == signinTimedOut && !left:
			// The 10 min are up while the root shows: the refresh doesn't start it again (R26).
			return q2spot.Reply{Message: "refresh", Rows: []q2spot.Row{
				{Kind: "action", URI: "signin", Title: "Sign In from Your Phone", Subtitle: msg}}}
		case !active:
			d.signin.start(d.signinPort)
		}
		_, msg = d.signin.result()
		return q2spot.Reply{Message: "refresh", Rows: []q2spot.Row{{Kind: "remote", Title: "Sign In from Your Phone", Subtitle: msg}}}
	}
	if d.signin.active() {
		d.signin.stop()
	}
	d.state.Write(nil) // the QR page's last words, once it is left
	sel, serr := d.selection()
	m := d.manifest()
	var bytes int64
	for _, e := range m.Songs {
		bytes += e.Size
	}
	onq2 := plural(len(sel.Items), "item") + " · " + size(bytes)
	if serr != nil {
		onq2 = "Selection file is damaged"
	}
	rows := []q2spot.Row{d.libraryRow(sel), list("sync", "Sync", d.syncSubtitle(cl), ""),
		list("onq2", "On This Q2", onq2, ""),
		list("playlists", "Playlists", "", ""), list("albums", "Albums", "", ""), list("artists", "Artists", "", ""),
		q2spot.Row{Kind: "search", Title: "Search"}, list("settings", "Settings", "", "")}
	// Last, so the rows above keep their places under the cursor (M33).
	if e := d.stream.Err(); e != nil {
		op, h, code := "stream", 0, 0
		var ae *subsonic.APIError
		if errors.As(e, &ae) {
			op, h, code = ae.Op, ae.HTTP, ae.Code
		}
		rows = append(rows, info(e.Error()), list("details", "Details", "", detailsCtx(op, h, code, e.at, e.id)))
	}
	return q2spot.Reply{Message: "refresh", Rows: rows}
}

func (d *daemon) syncSubtitle(cl *subsonic.Client) string {
	snap := d.runner.Snapshot()
	switch snap.State {
	case "running":
		return fmt.Sprintf("Syncing · %d %%", snap.Permille/10)
	case "paused":
		return fmt.Sprintf("Paused · %d %%", snap.Permille/10)
	}
	d.mu.Lock()
	if !d.pinging && time.Since(d.pingAt) > pingEvery {
		d.pinging = true
		d.spawn(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := cl.Ping(ctx)
			cancel()
			d.mu.Lock()
			d.pinging, d.pinged, d.pingErr, d.pingAt = false, true, err, time.Now()
			d.mu.Unlock()
		})
	}
	pinged, err, host := d.pinged, d.pingErr, d.host
	d.mu.Unlock()
	last := "Not synced yet"
	d.manifest()
	d.mu.Lock()
	at := d.manAt
	d.mu.Unlock()
	if !at.IsZero() {
		last = "Last synced " + when(at, time.Now())
	}
	switch {
	case q2.LocalIP() == "":
		return "Not connected · No Wi-Fi"
	case !pinged:
		return last
	case err != nil:
		return "Not connected · " + subsonic.Message(err, host)
	}
	return "Connected · " + last
}

func when(t, now time.Time) string {
	y, m, dd := now.Date()
	today := time.Date(y, m, dd, 0, 0, 0, 0, now.Location())
	switch {
	case !t.Before(today):
		return "today " + t.Format("15:04")
	case !t.Before(today.AddDate(0, 0, -1)):
		return "yesterday " + t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// libraryRow is Sync Entire Library: Off, On, or On with the counts once the preview has resolved it.
func (d *daemon) libraryRow(sel selection.Set) q2spot.Row {
	sub := "Off"
	if sel.Has(selection.KindLibrary, "all") {
		sub = "On"
		d.mu.Lock()
		c := d.libCount
		_, ok := d.resolved[selection.Key(selection.KindLibrary, "all")]
		d.mu.Unlock()
		if ok {
			sub = fmt.Sprintf("On · %s · %s", plural(c[0], "album"), plural(c[1], "playlist"))
		}
	}
	return q2spot.Row{Kind: "action", URI: "toggle", Title: "Sync Entire Library", Subtitle: sub,
		Context: "library:all", Position: "toggle"}
}

// onQ2 is the selection, each with its status and size on the card; a hold takes one out. Entire
// Library is switched from the root; here it shows only while on, as one of the items. Entire
// Library and a song, which have no page of their own, have their Quality here (I25).
func (d *daemon) onQ2(cursor string) q2spot.Reply {
	if _, err := d.selection(); err != nil {
		return damagedSelection()
	}
	v := d.view()
	var rows []q2spot.Row
	if len(v.sel.Items) == 0 {
		return q2spot.Reply{Rows: []q2spot.Row{{Kind: "info", Title: "Nothing chosen yet",
			Subtitle: "Hold Play/Pause on a playlist, album or artist"}}}
	}
	for _, it := range v.sel.Items {
		key := selection.Key(it.Kind, it.ID)
		quality := act("quality", "Quality: "+qualityName(it.Quality), key)
		if it.Kind == selection.KindLibrary {
			rows = append([]q2spot.Row{d.libraryRow(v.sel), quality}, rows...)
			continue
		}
		var b int64
		for _, sid := range v.keyed[key] {
			b += v.m.Songs[sid].Size
		}
		total := 0
		if it.Kind == selection.KindSong {
			total = 1
		}
		row := v.item(it.Kind, it.ID, it.Name, "", total)
		row.Subtitle = join(row.Subtitle, size(b))
		rows = append(rows, row)
		if it.Kind == selection.KindSong {
			rows[len(rows)-1].Kind, rows[len(rows)-1].URI = "info", ""
			rows = append(rows, quality)
		}
	}
	return page(rows, cursor)
}

var settingsMu sync.Mutex

func (d *daemon) settingsOp(op string) q2spot.Reply {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	s := d.settings()
	switch op {
	case "defq":
		s.DefaultQuality = nextQuality(s.def())
		d.saveSettings(s)
		return q2spot.Reply{Message: "Default Quality: " + qualityName(s.def())}
	case "streamq":
		s.StreamQuality = nextQuality(s.stream())
		d.saveSettings(s)
		return q2spot.Reply{Message: "Streaming Quality: " + qualityName(s.stream())}
	}
	d.mu.Lock()
	user := d.user
	d.mu.Unlock()
	return q2spot.Reply{Rows: []q2spot.Row{act("defq", "Default Quality: "+qualityName(s.def()), ""),
		act("streamq", "Streaming Quality: "+qualityName(s.stream()), ""), list("account", "Account", user, "")}}
}

func (d *daemon) account(cl *subsonic.Client) q2spot.Reply {
	d.mu.Lock()
	server, user := d.server, d.user
	d.mu.Unlock()
	if u, err := url.Parse(server); err == nil { // never a proxy's user:password@
		u.User = nil
		server = u.String()
	}
	stored := "Password is stored on this card"
	if cl.KeyAuth() {
		stored = "Only an API key is stored on this card"
	}
	return q2spot.Reply{Rows: []q2spot.Row{info(server), info(user), info(stored),
		info("Tip: use a server user just for the Q2"), act("signout", "Sign Out", "")}}
}
