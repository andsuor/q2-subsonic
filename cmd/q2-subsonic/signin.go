package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/andsuor/q2-subsonic/internal/creds"
	"github.com/andsuor/q2-subsonic/internal/q2"
	"github.com/andsuor/q2-subsonic/internal/qr"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
	"github.com/andsuor/q2-subsonic/internal/tlsroots"
)

//go:embed signin.html
var signinHTML []byte

const (
	signinCookie   = "q2sub"
	signinTimeout  = 10 * time.Minute
	pinTries       = 10
	pinLock        = time.Minute
	signinTimedOut = "Sign-in timed out"
)

// listenTCP listens on port, or on the next nine when it is taken; port 0 takes any free one.
func listenTCP(port int) (net.Listener, int) {
	n := 10
	if port == 0 {
		n = 1
	}
	for p := port; p < port+n; p++ {
		if l, err := net.Listen("tcp4", fmt.Sprintf(":%d", p)); err == nil {
			return l, l.Addr().(*net.TCPAddr).Port
		}
	}
	return nil, 0
}

// signin is the phone page for entering the Navidrome sign-in. It listens only between start and
// stop (a success, the Q2 leaving the QR page, or ten minutes), behind a one-time key (in the QR
// code) or the PIN on the Q2's screen, kept in an HttpOnly SameSite=Strict cookie. POSTs need X-Q2: 1.
type signin struct {
	dir       string
	state     interface{ Write(map[string]string) error }    // the app's state file (q2.State, or the daemon's merge)
	now       func() time.Time                               // default time.Now
	tick      time.Duration                                  // how often the 10 min limit is checked; default 1 s
	newClient func(subsonic.Creds) (*subsonic.Client, error) // default: the embedded roots

	mu       sync.Mutex
	ln       net.Listener
	srv      *http.Server
	done     chan struct{}
	key, pin string
	deadline time.Time
	fails    int
	lockout  time.Time
	ok       bool
	msg      string
	shown    map[string]string // the QR page's keys as start wrote them
}

func (s *signin) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *signin) active() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.ln != nil }

// result is how the last sign-in ended: whether it worked, and the words for the Q2's screen.
func (s *signin) result() (bool, string) { s.mu.Lock(); defer s.mu.Unlock(); return s.ok, s.msg }

// start opens the page and writes the QR code and PIN to the state file. It does nothing while open.
func (s *signin) start(port int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return
	}
	ln, p := listenTCP(port)
	if ln == nil {
		s.ok, s.msg = false, "Couldn't open the sign-in page"
		return
	}
	s.ln, s.done = ln, make(chan struct{})
	s.key, s.pin = q2.Key(16), q2.PIN(6)
	s.deadline = s.clock().Add(signinTimeout)
	s.fails, s.lockout, s.ok, s.msg = 0, time.Time{}, false, ""
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/api/login", s.login)
	mux.HandleFunc("/api/signin", s.signin)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	go s.srv.Serve(ln)
	go s.watch(s.done)

	kv := map[string]string{"pin": s.pin, "note": "Scan with your phone"}
	if ip := q2.LocalIP(); ip == "" {
		kv["addr"], kv["qr"], kv["qrsize"], kv["note"] = "", "", "0", "Connect the Q2 to Wi-Fi"
	} else {
		kv["addr"] = fmt.Sprintf("%s:%d", ip, p)
		kv["qr"], kv["qrsize"] = "", "0"
		if c, err := qr.Encode([]byte(fmt.Sprintf("http://%s/?k=%s", kv["addr"], s.key))); err == nil {
			kv["qr"], kv["qrsize"] = c.Hex(), fmt.Sprint(c.Size)
		}
	}
	s.shown = kv
	s.state.Write(kv)
}

// watch ends the sign-in when its ten minutes are up.
func (s *signin) watch(done chan struct{}) {
	d := s.tick
	if d == 0 {
		d = time.Second
	}
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			s.mu.Lock()
			late := !s.clock().Before(s.deadline)
			if late {
				s.ok, s.msg = false, signinTimedOut
			}
			s.mu.Unlock()
			if late {
				s.stop()
				return
			}
		}
	}
}

// stop closes the page and clears the keys it wrote from the state file.
func (s *signin) stop() {
	s.mu.Lock()
	if s.ln == nil {
		s.mu.Unlock()
		return
	}
	srv := s.srv
	s.ln, s.srv = nil, nil
	close(s.done)
	// After a success the Q2 may still show the QR page, which draws its note only beside a code
	// (addon.c qr_refresh; without addr it says "Connect the Q2 to Wi-Fi"): keep the code and
	// address, drop the PIN and say it worked. The server behind them is closed.
	kv := map[string]string{}
	if s.ok {
		kv = map[string]string{"addr": s.shown["addr"], "qr": s.shown["qr"], "qrsize": s.shown["qrsize"],
			"note": s.msg + " · Press Return"}
	}
	s.mu.Unlock()
	s.state.Write(kv)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if srv.Shutdown(ctx) != nil {
		srv.Close()
	}
}

// guess checks got against secret, counting a miss towards the lockout (pinTries wrong ones in a
// row lock every guess for a minute).
func (s *signin) guess(secret, got string) (ok, locked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	if now.Before(s.lockout) {
		return false, true
	}
	if got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1 {
		s.fails = 0
		return true, false
	}
	if s.fails++; s.fails >= pinTries {
		s.fails, s.lockout = 0, now.Add(pinLock)
	}
	return false, false
}

func (s *signin) authed(r *http.Request) bool {
	c, err := r.Cookie(signinCookie)
	s.mu.Lock()
	defer s.mu.Unlock()
	return err == nil && s.key != "" && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.key)) == 1
}

func (s *signin) setCookie(w http.ResponseWriter) {
	s.mu.Lock()
	v := s.key
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: signinCookie, Value: v, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func refuse(w http.ResponseWriter, code int, msg string) {
	reply(w, code, map[string]string{"error": msg})
}

func (s *signin) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	code, page := http.StatusOK, signinHTML
	if k := r.URL.Query().Get("k"); k != "" {
		s.mu.Lock()
		key := s.key
		s.mu.Unlock()
		if ok, locked := s.guess(key, k); ok {
			s.setCookie(w)
			http.Redirect(w, r, "/", http.StatusFound)
			return
		} else if locked {
			code = http.StatusTooManyRequests
		}
	}
	if !s.authed(r) {
		// The PIN form: the same page, with the sign-in form hidden.
		code, page = max(code, http.StatusUnauthorized), bytes.Replace(signinHTML, []byte(`data-auth="1"`), []byte(`data-auth="0"`), 1)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(code)
	w.Write(page)
}

// login takes the PIN from the Q2's screen.
func (s *signin) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("X-Q2") != "1" {
		refuse(w, http.StatusForbidden, "bad request")
		return
	}
	var body struct{ PIN string }
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body)
	s.mu.Lock()
	pin := s.pin
	s.mu.Unlock()
	ok, locked := s.guess(pin, strings.TrimSpace(body.PIN))
	switch {
	case locked:
		refuse(w, http.StatusTooManyRequests, "Too many tries. Wait a minute.")
	case !ok:
		refuse(w, http.StatusForbidden, "That is not the PIN on the Q2's screen.")
	default:
		s.setCookie(w)
		reply(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func (s *signin) signin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("X-Q2") != "1" {
		refuse(w, http.StatusForbidden, "bad request")
		return
	}
	if !s.authed(r) {
		refuse(w, http.StatusUnauthorized, "sign in")
		return
	}
	var c subsonic.Creds
	var body struct{ Server, User, Password string }
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil {
		refuse(w, http.StatusBadRequest, "bad body")
		return
	}
	c = subsonic.Creds{Server: strings.TrimSpace(body.Server), User: strings.TrimSpace(body.User), Password: body.Password}
	fin := func(ok bool, msg string) {
		s.mu.Lock()
		s.ok, s.msg = ok, msg
		s.mu.Unlock()
		reply(w, http.StatusOK, map[string]any{"ok": ok, "message": msg})
	}
	host := c.Server
	if u, err := url.Parse(c.Server); err == nil && u.Host != "" {
		host = u.Host
	}
	tlsroots.SetDateURL(c.Server) // the ping's certificate check needs a real clock (an unset Q2's isn't)
	mk := s.newClient
	if mk == nil {
		mk = func(c subsonic.Creds) (*subsonic.Client, error) {
			return subsonic.New(c, tlsroots.Client(15*time.Second))
		}
	}
	cl, err := mk(c)
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		c, err = cl.SignIn(ctx) // with OpenSubsonic API keys, only the key is kept
	}
	if err != nil {
		fin(false, subsonic.Message(err, host))
		return
	}
	if err := creds.Save(s.dir, c); err != nil {
		fin(false, "Couldn't save the sign-in")
		return
	}
	fin(true, "Signed in as "+c.User)
	go s.stop()
}
