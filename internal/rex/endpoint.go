package rex

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// Endpoint is one way to reach a Rex server: its Unix socket, or a TCP
// listener with or without TLS. A server exposes TCP listeners with
// `rex server run --listen`/`--extra-listen`, or over the tailnet with
// `rex server tailscale on`, which TLS-serves the API on the node's
// tailnet name, port 443.
type Endpoint struct {
	Unix string // the socket path, when the endpoint is local
	TLS  bool   // TCP endpoints: whether the listener speaks TLS
	Host string // TCP endpoints: host and port, as host:port
}

// String describes the endpoint for a user.
func (e Endpoint) String() string {
	if e.Unix != "" {
		return e.Unix
	}
	if e.TLS {
		return "https://" + e.Host
	}
	return "http://" + e.Host
}

// spec parses one server address as the -server option takes it. The
// boolean is false when the spec needs probing, because it names no
// transport: a bare host or host:port.
func spec(s string) (endpoints []Endpoint, probe bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return []Endpoint{{Unix: SocketPath()}}, false, nil
	}
	if strings.HasPrefix(s, "/") {
		return []Endpoint{{Unix: s}}, false, nil
	}
	// Only "scheme://" spells a scheme: host:port parses with "host" as
	// the scheme, and means a bare host with a port.
	if strings.Contains(s, "://") {
		u, perr := url.Parse(s)
		if perr != nil {
			return nil, false, fmt.Errorf("server endpoint %q is malformed", s)
		}
		switch u.Scheme {
		case "unix":
			if u.Host != "" && u.Host != "localhost" {
				return nil, false, fmt.Errorf("unix endpoint %q must not name a host", s)
			}
			return []Endpoint{{Unix: u.Path}}, false, nil
		case "http", "ws":
			return []Endpoint{{TLS: false, Host: hostPort(u)}}, false, nil
		case "https", "wss":
			return []Endpoint{{TLS: true, Host: hostPort(u)}}, false, nil
		default:
			return nil, false, fmt.Errorf("server endpoint scheme %q is not one of unix, http, https", u.Scheme)
		}
	}
	host, port, hpErr := net.SplitHostPort(s)
	if hpErr != nil {
		// No port: probe HTTPS on 443, then on Rex's tailnet port, then
		// plain TCP on it.
		return []Endpoint{
			{TLS: true, Host: net.JoinHostPort(s, "443")},
			{TLS: true, Host: net.JoinHostPort(s, "7297")},
			{TLS: false, Host: net.JoinHostPort(s, "7297")},
		}, true, nil
	}
	if host == "" {
		return nil, false, fmt.Errorf("server endpoint %q has no host", s)
	}
	// A port decides nothing about TLS: try it, then plain TCP.
	return []Endpoint{
		{TLS: true, Host: net.JoinHostPort(host, port)},
		{TLS: false, Host: net.JoinHostPort(host, port)},
	}, true, nil
}

// hostPort renders u as host:port, adding the scheme's default port when
// it names none.
func hostPort(u *url.URL) string {
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https", "wss":
			port = "443"
		case "http", "ws":
			port = "80"
		}
	}
	return net.JoinHostPort(host, port)
}

// Remote is true when the endpoint reaches a server over TCP rather than
// the machine's socket.
func (e Endpoint) Remote() bool { return e.Unix == "" }

// Name returns the endpoint's host without its port, a readable name for
// a remote server.
func (e Endpoint) Name() string {
	if e.Unix != "" {
		return ""
	}
	host, _, err := net.SplitHostPort(e.Host)
	if err != nil {
		return e.Host
	}
	return strings.TrimSuffix(host, ".")
}

// SpecEnv returns the server address the environment names, "" when it
// doesn't: the caller's flag or the local socket then decides.
func SpecEnv() string {
	return os.Getenv("REX_SERVER")
}
