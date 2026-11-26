package rex

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"

	"github.com/coder/websocket"
)

// dialWS opens a WebSocket at path on the server's endpoint, negotiating
// subprotocol. TLS endpoints verify against the system's roots: a
// tailnet server carries a real certificate for its ts.net name, which
// the host part must spell for the handshake to verify.
func dialWS(ctx context.Context, ep Endpoint, path, subprotocol string) (*websocket.Conn, error) {
	tr := &http.Transport{}
	var base string
	if ep.Unix != "" {
		base = "ws://localhost"
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", ep.Unix)
		}
	} else if ep.Pipe != "" {
		base = "ws://localhost"
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialPipe(ctx, ep.Pipe)
		}
	} else if ep.TLS {
		base = "wss://" + ep.Host
		tr.TLSClientConfig = &tls.Config{}
	} else {
		base = "ws://" + ep.Host
	}
	c, _, err := websocket.Dial(ctx, base+path, &websocket.DialOptions{
		Subprotocols: []string{subprotocol},
		HTTPClient:   &http.Client{Transport: tr},
	})
	return c, err
}
