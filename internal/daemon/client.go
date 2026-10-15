package daemon

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/Dudude-bit/ccswap/internal/api"
)

// Dial returns the generated API client talking to the daemon over its unix socket. Callers bound
// each call with a context; the client's own timeout only caps the slowest (mutating) one.
func Dial(socketPath string) (*api.ClientWithResponses, error) {
	hc := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
	return api.NewClientWithResponses("http://ccswap", api.WithHTTPClient(hc))
}
