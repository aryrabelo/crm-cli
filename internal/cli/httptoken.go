package cli

import (
	"os"

	"github.com/jdanielnd/crm-cli/internal/httpguard"
	"github.com/jdanielnd/crm-cli/internal/model"
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
