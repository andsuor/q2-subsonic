// Package subsonic is a small OpenSubsonic client (Navidrome) using token auth over HTTPS only.
package subsonic

import (
	"cmp"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/andsuor/q2-subsonic/internal/applog"
)

// Creds is a sign-in: a password for token auth, or an OpenSubsonic API key (then Password is empty).
type Creds struct {
	Server, User string
	Password     string `json:",omitempty"`
	APIKey       string `json:",omitempty"`
}

type Client struct {
	c    Creds
	base *url.URL
	hc   *http.Client
	// Log, when set, receives one redacted line per request.
	Log  *applog.Log
	idle time.Duration // a body Read that waits longer fails; 0 is idleAfter
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
		req.Header.Del("Referer") // Go sets it to the last URL, token and salt included
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

// auth returns the query values common to every request: the API key, or a token with a fresh salt
// each time. A client with neither user nor key sends no credentials at all.
func (c *Client) auth() url.Values {
	v := url.Values{}
	switch {
	case c.c.APIKey != "":
		v.Set("apiKey", c.c.APIKey) // u must not be sent with it (code 43)
	case c.c.User != "" || c.c.Password != "":
		b := make([]byte, 6)
		rand.Read(b)
		salt := hex.EncodeToString(b)
		sum := md5.Sum([]byte(c.c.Password + salt))
		v.Set("u", c.c.User)
		v.Set("t", hex.EncodeToString(sum[:]))
		v.Set("s", salt)
	}
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

const (
	// maxBody caps an API answer: decoding takes about twice its size, and the Q2 has about 60 MB.
	maxBody = 8 << 20
	// idleAfter fails a body Read that gets no data for this long (a hung transcode or NAS).
	idleAfter = 30 * time.Second
)

// do sends one GET for u, from byte from when from > 0, and logs it redacted. Its body fails with
// ErrUnreachable once one Read waits longer than the idle time, so a stalled server can't hang a
// download; time between Reads (a consumer yielding to a stream) doesn't count.
func (c *Client) do(ctx context.Context, op, u string, from int64) (*http.Response, error) {
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		cancel()
		return nil, &APIError{Op: op, Msg: applog.Redact(err.Error()), kind: ErrUnreachable}
	}
	if from > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", from))
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		cancel()
		c.logf("GET %s -> %v", u, err)
		return nil, &APIError{Op: op, Msg: applog.Redact(err.Error()), kind: transportKind(err)}
	}
	c.logf("GET %s -> %d", u, resp.StatusCode)
	idle := c.idle
	if idle == 0 {
		idle = idleAfter
	}
	b := &idleBody{ReadCloser: resp.Body, op: op, idle: idle, cancel: cancel}
	b.t = time.AfterFunc(idle, func() { b.fired.Store(true); cancel() })
	b.t.Stop()
	resp.Body = b
	return resp, nil
}

// idleBody is a response body whose Reads each get idle to see data; then the request is cancelled.
type idleBody struct {
	io.ReadCloser
	op     string
	idle   time.Duration
	t      *time.Timer
	fired  atomic.Bool
	cancel context.CancelFunc
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.t.Reset(b.idle)
	n, err := b.ReadCloser.Read(p)
	b.t.Stop()
	if err != nil && b.fired.Load() {
		err = &APIError{Op: b.op, Msg: fmt.Sprintf("no data for %v", b.idle), kind: ErrStalled}
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.t.Stop()
	b.cancel()
	return b.ReadCloser.Close()
}

// call performs one API request and decodes the subsonic-response object into out (when not nil).
func (c *Client) call(ctx context.Context, op string, q url.Values, out any) error {
	resp, err := c.do(ctx, op, c.endpoint(op, q), 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) {
			return err
		}
		return &APIError{Op: op, HTTP: resp.StatusCode, Msg: applog.Redact(err.Error()), kind: ErrUnreachable}
	}
	if len(body) > maxBody {
		what := "Answer"
		if op == "getPlaylist" {
			what = "Playlist"
		}
		return &APIError{Op: op, HTTP: resp.StatusCode, Msg: what + " too large for the Q2"}
	}
	return decode(op, resp.StatusCode, body, out)
}

// decode reads a subsonic-response: an *APIError for a failed status or a body that isn't one,
// else nil with out (when not nil) filled. The body is decoded in place, never copied.
func decode(op string, status int, body []byte, out any) error {
	var env struct {
		R *struct {
			Status string `json:"status"`
			Error  struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"subsonic-response"`
	}
	if json.Unmarshal(body, &env) != nil || env.R == nil {
		if status < 200 || status > 299 {
			return &APIError{Op: op, HTTP: status, Msg: http.StatusText(status), kind: ErrUnreachable}
		}
		return &APIError{Op: op, HTTP: status, Msg: "not a Subsonic response", kind: ErrNotSubsonic}
	}
	if e := env.R.Error; env.R.Status != "ok" {
		return &APIError{Op: op, HTTP: status, Code: e.Code, Msg: e.Message, kind: codeKind(e.Code)}
	}
	if out != nil {
		// An interface holding a pointer decodes into what it points at.
		if err := json.Unmarshal(body, &struct {
			R any `json:"subsonic-response"`
		}{out}); err != nil {
			return &APIError{Op: op, Msg: err.Error(), kind: ErrNotSubsonic}
		}
	}
	return nil
}

// SignIn checks the sign-in and returns what to keep on the card. A server offering OpenSubsonic API
// keys gets one typed in the password field, and only the key is kept, with the user name the server
// gives for it; any other server gets the password, checked with token auth.
func (c *Client) SignIn(ctx context.Context) (Creds, error) {
	if !c.offers(ctx, "apiKeyAuthentication") {
		return c.c, c.Ping(ctx)
	}
	k := *c
	k.c = Creds{Server: c.c.Server, APIKey: c.c.Password}
	var r struct {
		T struct {
			Username string `json:"username"`
		} `json:"tokenInfo"`
	}
	if err := k.call(ctx, "tokenInfo", nil, &r); err != nil {
		return Creds{}, err
	}
	k.c.User = cmp.Or(r.T.Username, c.c.User)
	return k.c, nil
}

// offers reports whether the server lists the OpenSubsonic extension. The call is public, so it
// carries no credentials; a server without it (plain Subsonic) offers nothing.
func (c *Client) offers(ctx context.Context, ext string) bool {
	anon := *c
	anon.c = Creds{Server: c.c.Server}
	var r struct {
		Ext []struct {
			Name string `json:"name"`
		} `json:"openSubsonicExtensions"`
	}
	if anon.call(ctx, "getOpenSubsonicExtensions", nil, &r) != nil {
		return false
	}
	for _, e := range r.Ext {
		if e.Name == ext {
			return true
		}
	}
	return false
}

// KeyAuth reports whether the client signs in with an API key rather than a password.
func (c *Client) KeyAuth() bool { return c.c.APIKey != "" }

func (c *Client) Ping(ctx context.Context) error { return c.call(ctx, "ping", nil, nil) }

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
	if err := c.call(ctx, "getPlaylists", nil, &r); err != nil {
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
	if err := c.call(ctx, "getPlaylist", url.Values{"id": {id}}, &r); err != nil {
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
	if err := c.call(ctx, "getAlbumList2", q, &r); err != nil {
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
	if err := c.call(ctx, "getAlbum", url.Values{"id": {id}}, &r); err != nil {
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
	if err := c.call(ctx, "getArtists", nil, &r); err != nil {
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
	if err := c.call(ctx, "getArtist", url.Values{"id": {id}}, &r); err != nil {
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
	if err := c.call(ctx, "search3", v, &r); err != nil {
		return nil, nil, nil, err
	}
	return artists(r.R.Artist), albums(r.R.Album), songs(r.R.Song), nil
}

func (c *Client) Song(ctx context.Context, id string) (Song, error) {
	var r struct {
		Song wireSong `json:"song"`
	}
	if err := c.call(ctx, "getSong", url.Values{"id": {id}}, &r); err != nil {
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

// Get fetches u, resuming at byte from when from > 0. The caller closes the body. A Subsonic error
// sent as the body (servers answer 200 with one) is returned as an error, so it is never saved as
// a song or a cover. An original the server won't let this user download (code 50) is fetched with
// stream?format=raw instead, which serves the same file untouched.
func (c *Client) Get(ctx context.Context, u string, from int64) (*http.Response, error) {
	op, id := "get", ""
	if pu, err := url.Parse(u); err == nil && pu.Path != "" {
		op, id = path.Base(pu.Path), pu.Query().Get("id")
	}
	resp, err := c.do(ctx, op, u, from)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		resp.Body.Close()
		return nil, &APIError{Op: op, HTTP: resp.StatusCode, Msg: http.StatusText(resp.StatusCode), kind: ErrUnreachable}
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "application/json" && !strings.HasSuffix(mt, "/xml") {
		return resp, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	err = decode(op, resp.StatusCode, body, nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code == 0 {
		return nil, &APIError{Op: op, HTTP: resp.StatusCode, Msg: "The server sent an error instead of the file"}
	}
	if ae.Code == 50 && op == "download" {
		return c.Get(ctx, c.endpoint("stream", url.Values{"id": {id}, "format": {"raw"}}), from)
	}
	return nil, err
}

func transportKind(err error) error {
	switch {
	case errors.Is(err, ErrInsecure):
		return ErrInsecure
	case isCert(err):
		var ci x509.CertificateInvalidError
		if errors.As(err, &ci) && ci.Reason == x509.Expired && !timeKnown() {
			return ErrUnreachable // the clock is unset and the server's time couldn't be read
		}
		return ErrCert
	}
	return ErrUnreachable
}
