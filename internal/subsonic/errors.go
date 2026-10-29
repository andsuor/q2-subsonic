package subsonic

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"

	"github.com/andsuor/q2-subsonic/internal/tlsroots"
)

var (
	ErrAuth        = errors.New("wrong username or password")
	ErrUnreachable = errors.New("server unreachable")
	ErrCert        = errors.New("certificate not trusted")
	ErrNotSubsonic = errors.New("not a Subsonic server")
	ErrInsecure    = errors.New("https required")
	ErrAuthMethod  = errors.New("sign-in method not accepted")
	// ErrStalled is a body that stopped arriving; it is ErrUnreachable too.
	ErrStalled = fmt.Errorf("server stopped sending: %w", ErrUnreachable)
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

// codeKind is the Err value a Subsonic error code unwraps to: 40 and 44 are wrong credentials,
// 41-43 a sign-in method the server won't take for this user (an LDAP user, say).
func codeKind(code int) error {
	switch code {
	case 40, 44:
		return ErrAuth
	case 41, 42, 43:
		return ErrAuthMethod
	}
	return nil
}

// codeMsg is the copy for Subsonic error codes whose server message would mislead or blame the
// wrong side.
var codeMsg = map[int]string{
	20: "The server needs a newer version of this app",
	30: "The server's Subsonic API is too old for this app",
	41: "This server doesn't allow token sign-in for this user",
	42: "This server doesn't accept this sign-in method",
	43: "This server doesn't accept this sign-in method",
	44: "Invalid API key: enter one from your server as the password",
}

// timeKnown is tlsroots.Known, injectable for tests.
var timeKnown = tlsroots.Known

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
	if errors.As(err, &he) && codeMsg[he.Code] != "" {
		return codeMsg[he.Code]
	}
	if errors.As(err, &he) && he.Code == 0 && he.HTTP != 0 && (he.HTTP < 200 || he.HTTP > 299) && errors.Is(err, ErrUnreachable) {
		return fmt.Sprintf("The server answered HTTP %d", he.HTTP)
	}
	switch {
	case errors.Is(err, ErrAuth):
		return "Wrong username or password"
	case errors.Is(err, ErrCert):
		return "Certificate not trusted"
	case errors.Is(err, ErrStalled):
		return "The server stopped sending"
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
