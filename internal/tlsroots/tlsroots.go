// Package tlsroots gives HTTPS clients an embedded Mozilla CA bundle (the Q2 has none) and a
// trustworthy clock: the Q2's is often unset (1970/2000), which fails certificate checks, so when
// the system year is before 2025 the time comes from a server's HTTP Date header instead.
package tlsroots

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"net/http"
	"sync"
	"time"
)

//go:embed cacert.pem
var bundle []byte

var (
	once sync.Once
	pool *x509.CertPool

	mu      sync.Mutex
	dateURL string
	offset  time.Duration // server time minus system time, once learned
	have    bool
	nowFn   = time.Now // injectable for tests
)

// Pool is the embedded Mozilla roots.
func Pool() *x509.CertPool {
	once.Do(func() {
		pool = x509.NewCertPool()
		pool.AppendCertsFromPEM(bundle)
	})
	return pool
}

// SetDateURL sets the URL whose Date header supplies the time when the system clock is unset.
func SetDateURL(u string) {
	mu.Lock()
	dateURL, have = u, false
	mu.Unlock()
}

// Now is the system time when its year is 2025 or later, else the server's time (cached as an offset
// from the system clock). With no usable server it falls back to the system time.
func Now() time.Time {
	sys := nowFn()
	if sys.Year() >= 2025 {
		return sys
	}
	mu.Lock()
	u, ok := dateURL, have
	off := offset
	mu.Unlock()
	if !ok && u != "" {
		// The Date header comes from a handshake we cannot verify yet, so it is only trusted for the
		// clock; certificate checking against this time still happens on every real request.
		c := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} // ponytail: Date read unverified; ceiling is a spoofed clock on a hostile LAN
		if resp, err := c.Head(u); err == nil {
			resp.Body.Close()
			if d, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
				off = d.Sub(sys)
				mu.Lock()
				offset, have = off, true
				mu.Unlock()
			}
		}
	}
	return sys.Add(off)
}

// Client is an HTTPS client verifying against Pool() at Now().
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: Pool(), Time: Now, MinVersion: tls.VersionTLS12},
	}}
}
