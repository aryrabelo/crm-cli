package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Cubic #4404329827: serveOptions accepts a bind beyond this machine only
// with TLS or --behind-tls-proxy; the subprocess tests cannot reach the
// success path because the server blocks.
func TestTLSFlags_ServeOptions(t *testing.T) {
	cases := []struct {
		name    string
		flags   tlsFlags
		addr    string
		wantErr string
		wantTLS bool
		wantOpt int
	}{
		{"loopback plain", tlsFlags{}, "127.0.0.1:8787", "", false, 0},
		{"beyond machine plain", tlsFlags{}, "0.0.0.0:8787", "clear text", false, 0},
		{"beyond machine behind proxy", tlsFlags{behindProxy: true}, "0.0.0.0:8787", "", false, 0},
		{"loopback behind proxy", tlsFlags{behindProxy: true}, "127.0.0.1:8787", "", false, 0},
		{"cert without key", tlsFlags{cert: "c.pem"}, "127.0.0.1:8787", "go together", false, 0},
		{"key without cert", tlsFlags{key: "k.pem"}, "0.0.0.0:8787", "go together", false, 0},
		{"unreadable pair", tlsFlags{cert: "nope.pem", key: "nope.key"}, "0.0.0.0:8787", "--tls-cert/--tls-key", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, isTLS, err := tc.flags.serveOptions(tc.addr, "--addr")
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantTLS, isTLS)
			assert.Len(t, opts, tc.wantOpt)
		})
	}
}
