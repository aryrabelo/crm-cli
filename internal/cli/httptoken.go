package cli

import (
	"crypto/tls"
	"os"

	"github.com/jdanielnd/crm-cli/internal/httpguard"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/spf13/pflag"
)

// minRemoteTokenLen is the shortest bearer token accepted on an address that
// is reachable beyond this machine; a generated token is far longer.
const minRemoteTokenLen = 32

// resolveHTTPToken returns the bearer token for an HTTP server on addr: the
// flag value, else $envName, else (loopback only) a generated one. flag names
// the address flag in error messages. A non-loopback address refuses to start
// without a token of at least minRemoteTokenLen characters.
func resolveHTTPToken(addr, flag, flagToken, envName string) (token string, generated bool, err error) {
	token = flagToken
	if token == "" {
		token = os.Getenv(envName)
	}
	loopback := httpguard.IsLoopback(addr)
	switch {
	case token == "" && !loopback:
		return "", false, model.NewExitError(model.ErrValidation,
			"%s %s listens beyond this machine: set --token or %s (or bind 127.0.0.1)", flag, addr, envName)
	case token != "" && !loopback && len(token) < minRemoteTokenLen:
		return "", false, model.NewExitError(model.ErrValidation,
			"%s %s listens beyond this machine: the token must be at least %d characters (try: openssl rand -hex 32)", flag, addr, minRemoteTokenLen)
	case token == "":
		t, err := httpguard.NewToken()
		if err != nil {
			return "", false, err
		}
		return t, true, nil
	}
	return token, false, nil
}

// tlsFlags are the --tls-cert, --tls-key and --behind-tls-proxy flags of an
// HTTP server command.
type tlsFlags struct {
	cert, key   string
	behindProxy bool
}

func (t *tlsFlags) register(f *pflag.FlagSet) {
	f.StringVar(&t.cert, "tls-cert", "", "serve HTTPS with this PEM certificate (needs --tls-key)")
	f.StringVar(&t.key, "tls-key", "", "PEM private key of --tls-cert")
	f.BoolVar(&t.behindProxy, "behind-tls-proxy", false, "allow a plain-HTTP bind beyond this machine because a TLS proxy in front terminates HTTPS")
}

// serveOptions validates the flags for a server on addr and returns the
// httpguard options to serve it, plus whether it serves HTTPS. The bearer
// token crosses the network in clear text over plain HTTP, so a bind beyond
// this machine needs TLS (or --behind-tls-proxy).
func (t *tlsFlags) serveOptions(addr, flag string) ([]httpguard.ServeOption, bool, error) {
	if (t.cert == "") != (t.key == "") {
		return nil, false, model.NewExitError(model.ErrValidation, "--tls-cert and --tls-key go together")
	}
	if t.cert != "" {
		if _, err := tls.LoadX509KeyPair(t.cert, t.key); err != nil {
			return nil, false, model.NewExitError(model.ErrValidation, "--tls-cert/--tls-key: %s", err)
		}
		return []httpguard.ServeOption{httpguard.WithTLS(t.cert, t.key)}, true, nil
	}
	if !httpguard.IsLoopback(addr) && !t.behindProxy {
		return nil, false, model.NewExitError(model.ErrValidation,
			"%s %s listens beyond this machine over plain HTTP, which sends the token in clear text: "+
				"set --tls-cert and --tls-key, or --behind-tls-proxy when a TLS proxy in front terminates HTTPS", flag, addr)
	}
	return nil, false, nil
}
