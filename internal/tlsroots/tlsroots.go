// Package tlsroots gives HTTPS clients an embedded Mozilla CA bundle (the Q2 has none) and a
// trustworthy clock: the Q2's is often unset (1970/2000), which fails certificate checks, so when
// the system year is before 2025 the time comes from a server's HTTP Date header instead.
package tlsroots

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

//go:embed cacert.pem
var bundle []byte

const retryAfter = 30 * time.Second

var (
	once sync.Once
	pool *x509.CertPool

	mu       sync.Mutex
	dateURL  string
	offset   time.Duration // server time minus system time, once learned
	have     bool
	fetching bool
	lastTry  time.Time
	nowFn    = time.Now // injectable for tests

	transports = map[time.Duration]*http.Transport{}
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
	dateURL, have, lastTry, offset = u, false, time.Time{}, 0
	mu.Unlock()
}

// Now is the system time when its year is 2025 or later, else the server's time (cached as an offset
// from the system clock). Until the server's time is known, and for 30 s after a failed fetch, it
// is the system time, never a wait. One fetch runs at a time.
func Now() time.Time {
	sys := nowFn()
	if sys.Year() >= 2025 {
		return sys
	}
	mu.Lock()
	if have || dateURL == "" || fetching || (!lastTry.IsZero() && sys.Sub(lastTry) < retryAfter) {
		off := offset
		mu.Unlock()
		return sys.Add(off)
	}
	fetching, lastTry = true, sys
	u := dateURL
	mu.Unlock()

	d, err := fetchDate(u)
	mu.Lock()
	fetching = false
	if err == nil {
		offset, have = d.Sub(sys), true
	}
	off := offset
	mu.Unlock()
	return sys.Add(off)
}

// fetchDate reads u's Date header. The handshake is fully verified (chain to Pool() and host name);
// only the validity period is judged at the leaf's own NotBefore, as the time is what is being asked.
func fetchDate(u string) (time.Time, error) {
	c := &http.Client{Timeout: 5 * time.Second, Transport: dateTransport(Pool())}
	resp, err := c.Head(u)
	if err != nil {
		return time.Time{}, err
	}
	resp.Body.Close()
	return http.ParseTime(resp.Header.Get("Date"))
}

func dateTransport(roots *x509.CertPool) *http.Transport {
	return &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			// Go can only run its own checks at a fixed Time, so verify here instead; this is the
			// documented pattern for custom verification, and it checks everything but the date.
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("tlsroots: no certificate")
				}
				leaf := cs.PeerCertificates[0]
				inter := x509.NewCertPool()
				for _, c := range cs.PeerCertificates[1:] {
					inter.AddCert(c)
				}
				_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter,
					DNSName: cs.ServerName, CurrentTime: leaf.NotBefore.Add(time.Second)})
				return err
			},
		},
	}
}

// Client is an HTTPS client verifying against Pool() at Now(). The timeout bounds connecting, the
// handshake and the wait for headers, not the body, so long downloads and streams are not cut.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Transport: transport(Pool(), timeout)}
}

func transport(roots *x509.CertPool, timeout time.Duration) *http.Transport {
	mu.Lock()
	defer mu.Unlock()
	return newTransport(roots, timeout)
}

func newTransport(roots *x509.CertPool, timeout time.Duration) *http.Transport {
	if t := transports[timeout]; t != nil && roots == pool {
		return t
	}
	t := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		TLSClientConfig:       &tls.Config{RootCAs: roots, Time: Now, MinVersion: tls.VersionTLS12},
	}
	if roots == pool {
		transports[timeout] = t
	}
	return t
}
