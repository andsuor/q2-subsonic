package subsonic

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
)

var (
	ErrAuth        = errors.New("wrong username or password")
	ErrUnreachable = errors.New("server unreachable")
	ErrCert        = errors.New("certificate not trusted")
	ErrNotSubsonic = errors.New("not a Subsonic server")
	ErrInsecure    = errors.New("https required")
)

// APIError keeps what the Details row shows. It unwraps to one of the Err values when the
// failure has a known kind.
type APIError struct {
	Op   string
	HTTP int
	Code int
	Msg  string
	kind error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: http %d code %d: %s", e.Op, e.HTTP, e.Code, e.Msg)
}
func (e *APIError) Unwrap() error { return e.kind }

// isCert reports whether err is a certificate verification failure.
func isCert(err error) bool {
	var ua x509.UnknownAuthorityError
	var ci x509.CertificateInvalidError
	var he x509.HostnameError
	var cv *tls.CertificateVerificationError
	return errors.As(err, &ua) || errors.As(err, &ci) || errors.As(err, &he) || errors.As(err, &cv)
}

// Message maps err to the user-facing sign-in copy.
func Message(err error, host string) string {
	var he *APIError
	if errors.As(err, &he) && he.Code == 0 && he.HTTP != 0 && (he.HTTP < 200 || he.HTTP > 299) && errors.Is(err, ErrUnreachable) {
		return fmt.Sprintf("Navidrome answered HTTP %d", he.HTTP)
	}
	switch {
	case errors.Is(err, ErrAuth):
		return "Wrong username or password"
	case errors.Is(err, ErrCert):
		return "Certificate not trusted"
	case errors.Is(err, ErrNotSubsonic):
		return "Not a Subsonic server"
	case errors.Is(err, ErrUnreachable):
		return "Can't reach " + host
	case errors.Is(err, ErrInsecure):
		return "HTTPS is required. Use an https:// address"
	}
	var ae *APIError
	if errors.As(err, &ae) && ae.Msg != "" {
		return ae.Msg
	}
	return err.Error()
}
