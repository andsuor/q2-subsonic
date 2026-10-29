// Package subsonic is a small OpenSubsonic client (Navidrome) using token auth over HTTPS only.
package subsonic

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/andsuor/q2-subsonic/internal/applog"
)

type Creds struct{ Server, User, Password string }

type Client struct {
	c    Creds
	base *url.URL
	hc   *http.Client
	// Log, when set, receives one redacted line per request.
	Log *applog.Log
}

// New refuses anything but an https server address.
func New(c Creds, hc *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(c.Server), "/"))
	if err != nil || u.Host == "" {
		return nil, &APIError{Op: "new", Msg: "bad server address", kind: ErrUnreachable}
	}
	if u.Scheme != "https" {
		return nil, ErrInsecure
	}
	h := *hc
	h.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return ErrInsecure
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return nil
	}
	return &Client{c: c, base: u, hc: &h}, nil
}

// auth returns the query values common to every request: a fresh salt and token each time.
func (c *Client) auth() url.Values {
	b := make([]byte, 6)
	rand.Read(b)
	salt := hex.EncodeToString(b)
	sum := md5.Sum([]byte(c.c.Password + salt))
	v := url.Values{}
	v.Set("u", c.c.User)
	v.Set("t", hex.EncodeToString(sum[:]))
	v.Set("s", salt)
	v.Set("v", "1.13.0") // token auth's version; a higher one makes Airsonic refuse every request (code 30)
	v.Set("c", "q2-subsonic")
	v.Set("f", "json")
	return v
}

func (c *Client) endpoint(op string, q url.Values) string {
	v := c.auth()
	for k, vs := range q {
		v[k] = vs
	}
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + "/rest/" + op
	u.RawQuery = v.Encode()
	return u.String()
}

func (c *Client) logf(format string, a ...any) {
	if c.Log != nil {
		c.Log.Printf("%s", applog.Redact(fmt.Sprintf(format, a...)))
	}
}

// call performs one API request and returns the inner subsonic-response object.
func (c *Client) call(ctx context.Context, op string, q url.Values) (json.RawMessage, error) {
	u := c.endpoint(op, q)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, &APIError{Op: op, Msg: applog.Redact(err.Error()), kind: ErrUnreachable}
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.logf("GET %s -> %v", u, err)
		return nil, &APIError{Op: op, Msg: applog.Redact(err.Error()), kind: transportKind(err)}
	}
	defer resp.Body.Close()
	c.logf("GET %s -> %d", u, resp.StatusCode)
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, &APIError{Op: op, HTTP: resp.StatusCode, Msg: applog.Redact(err.Error()), kind: ErrUnreachable}
	}
	var env struct {
		R json.RawMessage `json:"subsonic-response"`
	}
	if json.Unmarshal(body, &env) != nil || env.R == nil {
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, &APIError{Op: op, HTTP: resp.StatusCode, Msg: http.StatusText(resp.StatusCode), kind: ErrUnreachable}
		}
		return nil, &APIError{Op: op, HTTP: resp.StatusCode, Msg: "not a Subsonic response", kind: ErrNotSubsonic}
	}
	var st struct {
		Status string `json:"status"`
		Error  struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(env.R, &st) != nil {
		return nil, &APIError{Op: op, HTTP: resp.StatusCode, Msg: "bad response", kind: ErrNotSubsonic}
	}
	if st.Status != "ok" {
		return nil, &APIError{Op: op, HTTP: resp.StatusCode, Code: st.Error.Code, Msg: st.Error.Message, kind: codeKind(st.Error.Code)}
	}
	return env.R, nil
}

// get calls op and decodes the response object into out.
func (c *Client) get(ctx context.Context, op string, q url.Values, out any) error {
	raw, err := c.call(ctx, op, q)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &APIError{Op: op, Msg: err.Error(), kind: ErrNotSubsonic}
	}
	return nil
}

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.call(ctx, "ping", nil)
	return err
}

func (c *Client) Playlists(ctx context.Context) ([]Playlist, error) {
	var r struct {
		Playlists struct {
			Playlist []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				SongCount int    `json:"songCount"`
			} `json:"playlist"`
		} `json:"playlists"`
	}
	if err := c.get(ctx, "getPlaylists", nil, &r); err != nil {
		return nil, err
	}
	out := make([]Playlist, len(r.Playlists.Playlist))
	for i, p := range r.Playlists.Playlist {
		out[i] = Playlist(p)
	}
	return out, nil
}

// Playlist returns the playlist's songs in order, duplicates included.
func (c *Client) Playlist(ctx context.Context, id string) (Playlist, []Song, error) {
	var r struct {
		Playlist struct {
			ID        string     `json:"id"`
			Name      string     `json:"name"`
			SongCount int        `json:"songCount"`
			Entry     []wireSong `json:"entry"`
		} `json:"playlist"`
	}
	if err := c.get(ctx, "getPlaylist", url.Values{"id": {id}}, &r); err != nil {
		return Playlist{}, nil, err
	}
	p := r.Playlist
	return Playlist{p.ID, p.Name, p.SongCount}, songs(p.Entry), nil
}

func (c *Client) Albums(ctx context.Context, offset, size int) ([]Album, error) {
	var r struct {
		L struct {
			Album []wireAlbum `json:"album"`
		} `json:"albumList2"`
	}
	q := url.Values{"type": {"alphabeticalByName"}, "offset": {strconv.Itoa(offset)}, "size": {strconv.Itoa(size)}}
	if err := c.get(ctx, "getAlbumList2", q, &r); err != nil {
		return nil, err
	}
	return albums(r.L.Album), nil
}

func (c *Client) Album(ctx context.Context, id string) (Album, []Song, error) {
	var r struct {
		Album struct {
			wireAlbum
			Song []wireSong `json:"song"`
		} `json:"album"`
	}
	if err := c.get(ctx, "getAlbum", url.Values{"id": {id}}, &r); err != nil {
		return Album{}, nil, err
	}
	return r.Album.album(), songs(r.Album.Song), nil
}

func (c *Client) Artists(ctx context.Context) ([]Artist, error) {
	var r struct {
		Artists struct {
			Index []struct {
				Artist []wireArtist `json:"artist"`
			} `json:"index"`
		} `json:"artists"`
	}
	if err := c.get(ctx, "getArtists", nil, &r); err != nil {
		return nil, err
	}
	var out []Artist
	for _, ix := range r.Artists.Index {
		out = append(out, artists(ix.Artist)...)
	}
	return out, nil
}

func (c *Client) Artist(ctx context.Context, id string) (Artist, []Album, error) {
	var r struct {
		Artist struct {
			wireArtist
			Album []wireAlbum `json:"album"`
		} `json:"artist"`
	}
	if err := c.get(ctx, "getArtist", url.Values{"id": {id}}, &r); err != nil {
		return Artist{}, nil, err
	}
	return Artist(r.Artist.wireArtist), albums(r.Artist.Album), nil
}

// Search runs search3, 50 of each kind starting at offset.
func (c *Client) Search(ctx context.Context, q string, offset int) ([]Artist, []Album, []Song, error) {
	var r struct {
		R struct {
			Artist []wireArtist `json:"artist"`
			Album  []wireAlbum  `json:"album"`
			Song   []wireSong   `json:"song"`
		} `json:"searchResult3"`
	}
	o := strconv.Itoa(offset)
	v := url.Values{"query": {q}}
	for _, k := range []string{"artist", "album", "song"} {
		v.Set(k+"Count", "50")
		v.Set(k+"Offset", o)
	}
	if err := c.get(ctx, "search3", v, &r); err != nil {
		return nil, nil, nil, err
	}
	return artists(r.R.Artist), albums(r.R.Album), songs(r.R.Song), nil
}

func (c *Client) Song(ctx context.Context, id string) (Song, error) {
	var r struct {
		Song wireSong `json:"song"`
	}
	if err := c.get(ctx, "getSong", url.Values{"id": {id}}, &r); err != nil {
		return Song{}, err
	}
	return r.Song.song(), nil
}

// MediaURL returns the download URL for "original", else a stream URL transcoded to mp3
// (quality "mp3-320" or "mp3-128"). The URL embeds the auth token, so never log it unredacted.
func (c *Client) MediaURL(id, quality string) string {
	if quality == "original" {
		return c.endpoint("download", url.Values{"id": {id}})
	}
	rate := "128"
	if quality == "mp3-320" {
		rate = "320"
	}
	return c.endpoint("stream", url.Values{"id": {id}, "format": {"mp3"}, "maxBitRate": {rate}, "estimateContentLength": {"true"}})
}

func (c *Client) CoverURL(id string) string {
	return c.endpoint("getCoverArt", url.Values{"id": {id}})
}

// Get fetches u, resuming at byte from when from > 0. The caller closes the body.
func (c *Client) Get(ctx context.Context, u string, from int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, &APIError{Op: "get", Msg: applog.Redact(err.Error()), kind: ErrUnreachable}
	}
	if from > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", from))
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.logf("GET %s -> %v", u, err)
		return nil, &APIError{Op: "get", Msg: applog.Redact(err.Error()), kind: transportKind(err)}
	}
	c.logf("GET %s -> %d", u, resp.StatusCode)
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		resp.Body.Close()
		return nil, &APIError{Op: "get", HTTP: resp.StatusCode, Msg: http.StatusText(resp.StatusCode), kind: ErrUnreachable}
	}
	return resp, nil
}

func transportKind(err error) error {
	switch {
	case errors.Is(err, ErrInsecure):
		return ErrInsecure
	case isCert(err):
		return ErrCert
	}
	return ErrUnreachable
}
