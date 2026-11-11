package main

import (
	"cmp"
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/selection"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// Discover (the server's own lists, what is starred, genres), shuffle and radio, and an artist's
// Play, Shuffle and Top Songs. All of it is plain rows and the streamer: no firmware word is needed.

const (
	shuffleSongs = 100 // songs a Shuffle asks the server for
	radioSongs   = 50  // songs of a Radio, and of Top Songs
	// artistPlayIn is how long an artist's Play or Shuffle may fetch its albums' songs.
	artistPlayIn = time.Minute
	// maxGenre is the longest genre name listed, escaped: a row's context must fit the firmware's
	// 400 bytes of level arguments with its cursor, or a press on it does nothing.
	maxGenre = 300
	notHere  = "Not available on this server" // Top Songs or Radio on a server without it
)

func (d *daemon) discover() q2spot.Reply {
	d.mu.Lock()
	d.random = nil // drawn again when Random Albums is next opened
	d.mu.Unlock()
	return q2spot.Reply{Rows: []q2spot.Row{act("shuffle", "Shuffle All", ""),
		list("albumlist", "Recently Added", "", "newest"), list("albumlist", "Recently Played", "", "recent"),
		list("albumlist", "Most Played", "", "frequent"), list("albumlist", "Random Albums", "", "random"),
		list("starred", "Starred", "", ""), list("genres", "Genres", "", "")}}
}

func (v view) album(a subsonic.Album) q2spot.Row {
	return v.item(selection.KindAlbum, a.ID, a.Name, a.Artist, a.SongCount)
}

// albumList is one page of an album list the server pages (typ is getAlbumList2's type, genre
// byGenre's genre): head, then the albums. The cursor counts rows, head included, as withLead
// and fit set it; one album more than a page is asked for, which says whether More… follows. With
// "window" the reply is a window's (windowed).
func (d *daemon) albumList(ctx context.Context, cl *subsonic.Client, typ, genre string, head []q2spot.Row, cursor string) q2spot.Reply {
	cur, _ := strconv.Atoi(cursor)
	rows := q2spot.MaxRows
	if d.caps.Window {
		rows = winRoom(max(cur, 0))
	}
	off, room := 0, rows-len(head)
	if cur > 0 {
		off, room, head = max(cur-len(head), 0), rows, nil
	}
	al, err := cl.AlbumList(ctx, typ, genre, off, room+1)
	if err != nil {
		return d.listErr(err, cursor)
	}
	more := len(al) > room
	if more {
		al = al[:room]
	}
	v := d.view()
	r := q2spot.Reply{Rows: head}
	covers := map[string]string{}
	for _, a := range al {
		row := coverRow(v.album(a), covers, a.CoverArt)
		if typ == "byYear" && a.Year > 0 {
			row.Subtitle = join(strconv.Itoa(a.Year), row.Subtitle)
		}
		r.Rows = append(r.Rows, row)
	}
	if d.caps.Window {
		r = windowed(r.Rows, cur, max(cur, 0), more)
	} else if n := fit(r.Rows); n < len(r.Rows) {
		r.Rows, more = r.Rows[:n], true
	}
	if more && !d.caps.Window {
		r.Cursor = strconv.Itoa(max(cur, 0) + len(r.Rows))
	}
	if len(al) == 0 && cur <= 0 {
		r.Rows = append(r.Rows, info("No albums"))
	}
	return d.withArt(r, covers, cursor)
}

// albumLists is one of Discover's album lists. Random Albums is one page, drawn once and kept until
// Discover is opened again: the firmware asks for a level again after every press and Back, and
// the list must not change under the user.
func (d *daemon) albumLists(ctx context.Context, cl *subsonic.Client, typ, cursor string) q2spot.Reply {
	switch typ {
	case "newest", "recent", "frequent":
		return d.albumList(ctx, cl, typ, "", nil, cursor)
	case "random":
	default:
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	d.mu.Lock()
	al := d.random
	d.mu.Unlock()
	if al == nil {
		var err error
		if al, err = cl.AlbumList(ctx, typ, "", 0, q2spot.MaxRows); err != nil {
			return d.listErr(err, cursor)
		}
		d.mu.Lock()
		d.random = al
		d.mu.Unlock()
	}
	v := d.view()
	rows := []q2spot.Row{}
	covers := map[string]string{}
	for _, a := range al {
		rows = append(rows, coverRow(v.album(a), covers, a.CoverArt))
	}
	if len(rows) == 0 {
		rows = append(rows, info("No albums"))
	}
	return d.withArt(d.page(rows, cursor), covers, cursor)
}

// starredFound is what is starred, held for browseTTL only: a server may have thousands.
func (d *daemon) starredFound(ctx context.Context, cl *subsonic.Client) (found, error) {
	return cached(d, cl, "starred", func() (f found, err error) {
		f.ars, f.als, f.songs, err = cl.Starred(ctx)
		return f, err
	})
}

func (d *daemon) starred(ctx context.Context, cl *subsonic.Client, cursor string) q2spot.Reply {
	f, err := d.starredFound(ctx, cl)
	if err != nil {
		return d.listErr(err, cursor)
	}
	rows := d.foundRows(f, "starred:")
	if len(rows) == 0 {
		rows = []q2spot.Row{{Kind: "info", Title: "Nothing starred", Subtitle: "Star music on the server to see it here"}}
	}
	return d.page(rows, cursor)
}

// genres lists the server's genres by name; one opens its albums.
func (d *daemon) genres(ctx context.Context, cl *subsonic.Client, cursor string) q2spot.Reply {
	gs, err := cached(d, cl, "genres", func() ([]subsonic.Genre, error) {
		gs, err := cl.Genres(ctx)
		slices.SortFunc(gs, func(a, b subsonic.Genre) int {
			return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.Name, b.Name))
		})
		return gs, err
	})
	if err != nil {
		return d.listErr(err, cursor)
	}
	var rows []q2spot.Row
	for _, g := range gs {
		if g.Name == "" || len(q2spot.Escape(g.Name)) > maxGenre {
			continue
		}
		sub := ""
		if g.AlbumCount > 0 {
			sub = plural(g.AlbumCount, "album")
		}
		rows = append(rows, list("genre", g.Name, sub, g.Name))
	}
	if len(rows) == 0 {
		rows = []q2spot.Row{info("No genres")}
	}
	return d.page(rows, cursor)
}

// top is an artist's Top Songs. getTopSongs takes the artist's name, so the artist is asked for
// first (its page's answer, while that is kept).
func (d *daemon) top(ctx context.Context, cl *subsonic.Client, arg, cursor string) q2spot.Reply {
	_, id := parseKey(arg)
	key := "top:" + id
	songs, err := cached(d, cl, key, func() ([]subsonic.Song, error) {
		it, err := d.item(ctx, cl, selection.KindArtist, id)
		if err != nil {
			return nil, err
		}
		return cl.TopSongs(ctx, it.name, radioSongs)
	})
	switch {
	case subsonic.Unsupported(err):
		return q2spot.Reply{Rows: []q2spot.Row{info(notHere)}}
	case errors.Is(err, subsonic.ErrNotFound):
		songs = nil
	case err != nil:
		return d.listErr(err, cursor)
	}
	d.mu.Lock()
	d.listed = songList{key, songs}
	d.mu.Unlock()
	v := d.view()
	var rows []q2spot.Row
	for i, s := range songs {
		rows = append(rows, v.song(s, key, i))
	}
	if len(rows) == 0 {
		rows = []q2spot.Row{info("No top songs")}
	}
	return d.page(rows, cursor)
}

// playNow streams songs from the first, or says there is nothing.
func (d *daemon) playNow(songs []subsonic.Song, none string) q2spot.Reply {
	if len(songs) == 0 {
		return q2spot.Fail(q2spot.Unavailable, none)
	}
	d.startStream(d.nextPlay(), songs, 0)
	return q2spot.Reply{Message: "Playing " + songs[0].Title}
}

// shuffle plays random songs: of the whole server (""), of a genre ("genre:<name>"), or an
// artist's own in random order ("artist:<id>"). The argument is cut at its first colon only: a
// genre's name may hold spaces and colons.
func (d *daemon) shuffle(ctx context.Context, cl *subsonic.Client, arg string) q2spot.Reply {
	what, name, _ := strings.Cut(arg, ":")
	if what == string(selection.KindArtist) {
		return d.playArtist(ctx, cl, name, true)
	}
	if what != "genre" {
		name = ""
	}
	songs, err := cl.RandomSongs(ctx, shuffleSongs, name)
	if err != nil {
		return d.errToast(err)
	}
	return d.playNow(songs, "Nothing to play")
}

// radio plays songs like an artist's, as the server picks them.
func (d *daemon) radio(ctx context.Context, cl *subsonic.Client, arg string) q2spot.Reply {
	_, id := parseKey(arg)
	songs, err := cl.SimilarSongs(ctx, id, radioSongs)
	switch {
	case subsonic.Unsupported(err):
		return q2spot.Fail(q2spot.Unavailable, notHere)
	case errors.Is(err, subsonic.ErrNotFound): // Navidrome's answer when it knows none
		songs = nil
	case err != nil:
		return d.errToast(err)
	}
	return d.playNow(songs, "No similar songs found")
}

// playArtist plays every album of an artist, in order or shuffled. The reply asks only for the
// artist; the albums' songs are fetched behind it, as many albums don't fit one reply's time. A
// later play, or Sign Out, in that time wins: this one then neither starts nor leaves its error.
func (d *daemon) playArtist(ctx context.Context, cl *subsonic.Client, id string, shuffle bool) q2spot.Reply {
	it, err := d.item(ctx, cl, selection.KindArtist, id)
	if err != nil {
		return d.errToast(err)
	}
	if len(it.albums) == 0 {
		return q2spot.Fail(q2spot.Unavailable, "Nothing to play")
	}
	seq := d.nextPlay()
	d.spawn(func() {
		ctx, cancel := context.WithTimeout(context.Background(), artistPlayIn)
		defer cancel()
		var songs []subsonic.Song
		var err error
		for _, a := range it.albums {
			if !d.latestPlay(seq) {
				return
			}
			var s []subsonic.Song
			if _, s, err = getAlbum(ctx, cl, a.ID); err != nil {
				break
			}
			songs = append(songs, s...)
		}
		if err == nil && len(songs) == 0 {
			err = errors.New("no songs")
		}
		if shuffle {
			rand.Shuffle(len(songs), func(i, j int) { songs[i], songs[j] = songs[j], songs[i] })
		}
		d.playMu.Lock()
		defer d.playMu.Unlock()
		switch {
		case !d.latestPlay(seq):
		case err != nil: // shown on the root, as a stream that gave up is
			d.log.Printf("error: %v", err)
			d.stream.setErr(&streamErr{title: it.name, at: time.Now(), err: err})
		default:
			d.stream.start(songs, 0)
		}
	})
	if shuffle {
		return q2spot.Reply{Message: "Shuffling " + it.name}
	}
	return q2spot.Reply{Message: "Playing " + it.name}
}
