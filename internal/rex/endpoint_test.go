package rex

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSpec(t *testing.T) {
	cases := []struct {
		in    string
		want  []Endpoint
		probe bool
	}{
		{"/run/user/1000/rex/server.sock", []Endpoint{{Unix: "/run/user/1000/rex/server.sock"}}, false},
		{"unix:///run/user/1000/rex/server.sock", []Endpoint{{Unix: "/run/user/1000/rex/server.sock"}}, false},
		{"http://127.0.0.1:7297", []Endpoint{{Host: "127.0.0.1:7297"}}, false},
		{"https://rex.example.ts.net", []Endpoint{{TLS: true, Host: "rex.example.ts.net:443"}}, false},
		{"https://rex.example.ts.net:8443", []Endpoint{{TLS: true, Host: "rex.example.ts.net:8443"}}, false},
		{"rex.example.ts.net", []Endpoint{
			{TLS: true, Host: "rex.example.ts.net:443"},
			{TLS: true, Host: "rex.example.ts.net:7297"},
			{Host: "rex.example.ts.net:7297"},
		}, true},
		{"10.0.0.5", []Endpoint{
			{TLS: true, Host: "10.0.0.5:443"},
			{TLS: true, Host: "10.0.0.5:7297"},
			{Host: "10.0.0.5:7297"},
		}, true},
		{"host:9000", []Endpoint{
			{TLS: true, Host: "host:9000"},
			{Host: "host:9000"},
		}, true},
		{"[fd7a::1]:7297", []Endpoint{
			{TLS: true, Host: "[fd7a::1]:7297"},
			{Host: "[fd7a::1]:7297"},
		}, true},
		{`npipe:////./pipe/rex-S-1-5-18`, []Endpoint{{Pipe: `\\.\pipe\rex-S-1-5-18`}}, false},
		{`\\.\pipe\rex-S-1-5-18`, []Endpoint{{Pipe: `\\.\pipe\rex-S-1-5-18`}}, false},
		{`\\.\PIPE\rex-test`, []Endpoint{{Pipe: `\\.\pipe\rex-test`}}, false},
	}
	for _, c := range cases {
		eps, probe, err := spec(c.in)
		if err != nil {
			t.Errorf("spec(%q): %v", c.in, err)
			continue
		}
		if probe != c.probe {
			t.Errorf("spec(%q) probe: got %v, want %v", c.in, probe, c.probe)
		}
		if fmt.Sprint(eps) != fmt.Sprint(c.want) {
			t.Errorf("spec(%q): got %v, want %v", c.in, eps, c.want)
		}
	}
	for _, bad := range []string{"gopher://x", "ssh://host", ":9000", "foo://", `npipe:////./pipe/`, `\\.\pipe\`, `npipe:////host/pipe/rex`} {
		if _, _, err := spec(bad); err == nil {
			t.Errorf("spec(%q) should fail", bad)
		}
	}
	// "" means this machine's server: its socket on Unix, its pipe on
	// Windows, each as defaultEndpoint picks it.
	eps, _, err := spec("")
	if err != nil || len(eps) != 1 || eps[0] != defaultEndpoint() {
		t.Errorf("spec(\"\"): got %v, %v; want %v", eps, err, defaultEndpoint())
	}
	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(eps[0].Pipe, `\\.\pipe\rex-`) {
			t.Errorf("the Windows default is no rex pipe: %s", eps[0])
		}
	} else if eps[0].Unix != SocketPath() {
		t.Errorf("the Unix default is not the socket: %s vs %s", eps[0], SocketPath())
	}
}

// startTestServer runs a throwaway rex server with a plain TCP listener,
// as `rex server run --extra-listen http://… --allow-anonymous` serves.
// pipe is the Windows server's named pipe, "" elsewhere.
func startTestServer(t *testing.T) (addr, pipe string, stop func()) {
	t.Helper()
	if _, err := exec.LookPath("rex"); err != nil {
		t.Skip("rex is not on PATH")
	}
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	// Windows has no Unix socket path to listen on; its server takes a
	// pipe instead.
	listen := "unix://" + filepath.Join(dir, "sock")
	if runtime.GOOS == "windows" {
		pipe = fmt.Sprintf(`\\.\pipe\rex-test-%d`, os.Getpid())
		listen = fmt.Sprintf("npipe:////./pipe/rex-test-%d", os.Getpid())
	}
	cmd := exec.Command("rex", "server", "run",
		"--listen", listen,
		"--extra-listen", fmt.Sprintf("http://127.0.0.1:%d", port),
		"--allow-anonymous",
		"--state-dir", filepath.Join(dir, "state"),
		"--log-level", "error")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the test server: %v", err)
	}
	stop = func() {
		cmd.Process.Kill()
		cmd.Wait()
	}
	// The TCP listener answering says the server is ready to serve.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return fmt.Sprintf("127.0.0.1:%d", port), pipe, stop
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	t.Fatal("the test server did not come up")
	return "", "", nil
}

// TestConnectTCP connects to a TCP listener through the host:port probe:
// the HTTPS try fails fast, plain TCP answers.
func TestConnectTCP(t *testing.T) {
	addr, _, stop := startTestServer(t)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Connect(ctx, addr)
	if err != nil {
		t.Fatalf("Connect(%s): %v", addr, err)
	}
	defer c.Close()
	if c.Ep.TLS {
		t.Errorf("the probe settled on TLS for a plain listener: %s", c.Ep)
	}
	if !c.Ep.Remote() {
		t.Errorf("endpoint %s should be remote", c.Ep)
	}
	b, err := c.Bootstrap(ctx, []string{}...)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if b.ServerStatus.PID == 0 {
		t.Error("bootstrap reported no server status")
	}
	// A session's whole cycle works over TCP.
	sid, err := c.SessionCreate(ctx, "gorex-tcp-test")
	if err != nil {
		t.Fatalf("session.create: %v", err)
	}
	if err := c.SessionDestroy(ctx, sid); err != nil {
		t.Errorf("session.destroy: %v", err)
	}
}

// TestConnectExplicitScheme checks the scheme decides TLS without
// probing: http:// reaches the plain listener in one try.
func TestConnectExplicitScheme(t *testing.T) {
	addr, _, stop := startTestServer(t)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Connect(ctx, "http://"+addr)
	if err != nil {
		t.Fatalf("Connect(http://%s): %v", addr, err)
	}
	defer c.Close()
	if c.Ep.TLS {
		t.Errorf("http:// must not use TLS: %s", c.Ep)
	}
	if got := c.Ep.String(); got != "http://"+addr {
		t.Errorf("endpoint: got %q, want %q", got, "http://"+addr)
	}
}

// TestConnectPipe reaches the test server over its named pipe, the
// transport Windows's server uses, spelled the two ways spec takes it.
func TestConnectPipe(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("named pipes are Windows's transport")
	}
	_, pipe, stop := startTestServer(t)
	defer stop()
	for _, spec := range []string{pipe, "npipe://" + pipe} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		c, err := Connect(ctx, spec)
		cancel()
		if err != nil {
			t.Fatalf("Connect(%s): %v", spec, err)
		}
		if c.Ep.Pipe != pipe {
			t.Errorf("Connect(%s): endpoint %s is not the pipe %s", spec, c.Ep, pipe)
		}
		if c.Ep.Remote() {
			t.Errorf("the pipe endpoint %s is not remote", c.Ep)
		}
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		b, err := c.Bootstrap(ctx, []string{}...)
		cancel()
		if err != nil {
			t.Fatalf("bootstrap over the pipe: %v", err)
		}
		if b.ServerStatus.PID == 0 {
			t.Error("bootstrap reported no server status")
		}
		c.Close()
	}
}

// TestConnectRemote validates the client against a real remote server,
// read-only: REX_TEST_REMOTE names it the way -server would, such as a
// tailnet name after `rex server tailscale on` there. It attaches to no
// session and writes nothing.
func TestConnectRemote(t *testing.T) {
	spec := os.Getenv("REX_TEST_REMOTE")
	if spec == "" {
		t.Skip("REX_TEST_REMOTE names no server to test against")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	c, err := Connect(ctx, spec)
	if err != nil {
		t.Fatalf("Connect(%s): %v", spec, err)
	}
	defer c.Close()
	b, err := c.Bootstrap(ctx, []string{}...)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	t.Logf("server pid %d, api revision %d, %d sessions", b.ServerStatus.PID, b.ServerStatus.APIRevision, b.ServerStatus.SessionCount)
	sessions, err := c.SessionList(ctx)
	if err != nil {
		t.Fatalf("session.list: %v", err)
	}
	t.Logf("sessions: %v", sessions)
	// A stream of an existing terminal is the remote feature that matters:
	// decode its snapshot without touching sizes or input.
	for _, s := range sessions {
		v, _, err := c.Attach(ctx, s.SessionID)
		if err != nil {
			t.Logf("session %s: attach: %v", s.SessionID, err)
			continue
		}
		for _, w := range v.Windows {
			for _, layer := range w.Layers {
				for _, b := range layer.Blocks {
					if !strings.Contains(b.Flavor, "terminal") {
						continue
					}
					st := c.Stream(s.SessionID, b.BlockID, 0, 0)
					buf := make([]byte, 4096)
					n, err := st.Read(buf)
					st.Close()
					if err != nil && err != io.EOF {
						t.Errorf("block %s: stream: %v", b.BlockID, err)
						continue
					}
					if n == 0 {
						t.Errorf("block %s: the snapshot restored no output", b.BlockID)
						continue
					}
					t.Logf("block %s: stream restored %d bytes of snapshot", b.BlockID, n)
					return
				}
			}
		}
	}
	t.Log("no terminal block in any session to stream")
}
