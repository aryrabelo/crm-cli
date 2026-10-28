package cli

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/ui"
	"github.com/spf13/cobra"
)

// uiTokenEnv is the environment variable read when --token is not given.
const uiTokenEnv = "CRM_UI_TOKEN"

// defaultUIAddr is where crm ui listens by default.
const defaultUIAddr = "127.0.0.1:8787"

func registerUICommand(rootCmd *cobra.Command) {
	rootCmd.AddCommand(uiCmd())
}

func uiCmd() *cobra.Command {
	var addr, token string
	var open bool
	var tlsOpt tlsFlags
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Open the spreadsheet UI: tables, columns, estimates and live runs in the browser",
		Long: `Serve a spreadsheet view of your tables on a local web page (same binary,
no extra install): rows × columns, per-cell run status, add column, estimate
and run a column with a budget, live progress, CSV import and export.

Security: every API request needs the session token.
  - --token (or ` + uiTokenEnv + `) sets it. Without one, a loopback address gets a
    random token generated at startup; any other address refuses to start.
  - The URL printed on stderr carries the token in its #fragment (never sent
    to the server or written to logs); the page keeps it for that tab only.
  - On a loopback address only Host and Origin values naming 127.0.0.1,
    localhost or [::1] with the bound port are accepted (DNS rebinding), and
    state-changing requests must be same-origin JSON (or CSV) requests.
  - --tls-cert and --tls-key serve HTTPS. Plain HTTP sends the token in clear
    text, so a bind beyond this machine refuses to start without TLS unless
    --behind-tls-proxy says a TLS reverse proxy in front terminates HTTPS.

Runs started from the page execute in this process; Ctrl-C stops the server
and leaves them interrupted and resumable (crm run --resume). A crm run
started in another terminal shows up on the page while it executes.`,
		Example: `  crm ui                       # http://127.0.0.1:8787, token printed on stderr
  crm ui --open                # also open it in the default browser
  crm ui --addr 127.0.0.1:9000
  CRM_DB=/path/to/other.db crm ui`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, _, err := net.SplitHostPort(addr); err != nil {
				return model.NewExitError(model.ErrValidation, "--addr %q: want HOST:PORT", addr)
			}
			token, _, err := resolveHTTPToken(addr, "--addr", token, uiTokenEnv)
			if err != nil {
				return err
			}
			serveOpts, isTLS, err := tlsOpt.serveOptions(addr, "--addr")
			if err != nil {
				return err
			}
			scheme := "http"
			if isTLS {
				scheme = "https"
			}
			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			s := ui.New(db, provider.NewEnvRegistry())
			defer s.Close()
			return s.Serve(cmd.Context(), addr, token, func(a net.Addr) {
				if tlsOpt.behindProxy && !isTLS {
					// The backend is plain HTTP: its URL would send the token in
					// clear text, so open the proxy's HTTPS address instead.
					fmt.Fprintf(os.Stderr, "crm ui: serving plain HTTP on %s for a TLS proxy (Ctrl-C to stop)\n", browserHost(a))
					fmt.Fprintf(os.Stderr, "crm ui: open the proxy's HTTPS address with this fragment (it carries the session token; keep it private):\n    https://<proxy-host>/%s\n", pageFragment(token))
					if open {
						fmt.Fprintln(os.Stderr, "crm ui: --open skipped: the backend URL is plain HTTP; use the proxy's HTTPS address")
					}
					return
				}
				url := pageURL(scheme, a, token)
				fmt.Fprintf(os.Stderr, "crm ui: serving on %s://%s (Ctrl-C to stop)\n", scheme, browserHost(a))
				fmt.Fprintf(os.Stderr, "crm ui: open this URL (it carries the session token; keep it private):\n    %s\n", url)
				if open {
					if err := openBrowser(url); err != nil {
						fmt.Fprintf(os.Stderr, "crm ui: could not open a browser (%s); open the URL above\n", err)
					}
				}
			}, serveOpts...)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", defaultUIAddr, "listen on HOST:PORT")
	cmd.Flags().StringVar(&token, "token", "", "session token required on API requests (default $"+uiTokenEnv+", else generated on loopback)")
	tlsOpt.register(cmd.Flags())
	cmd.Flags().BoolVar(&open, "open", false, "open the page in the default browser")
	return cmd
}

// browserHost is the HOST:PORT a browser reaches a: an unspecified bind
// (0.0.0.0, ::) is reached through 127.0.0.1.
func browserHost(a net.Addr) string {
	tcp, ok := a.(*net.TCPAddr)
	if !ok {
		return a.String()
	}
	if tcp.IP.IsUnspecified() {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(tcp.Port))
	}
	return tcp.String()
}

// pageURL is the page URL with the token in its fragment.
func pageURL(scheme string, a net.Addr, token string) string {
	return scheme + "://" + browserHost(a) + "/" + pageFragment(token)
}

// pageFragment is the #token= fragment of the page URL.
func pageFragment(token string) string {
	return "#token=" + strings.ReplaceAll(url.QueryEscape(token), "+", "%20")
}

func openBrowser(target string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", target)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		c = exec.Command("xdg-open", target)
	}
	if err := c.Start(); err != nil {
		return err
	}
	return c.Process.Release()
}
