package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"gorex/internal/rex"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// startTCPTestServer runs a throwaway rex server with a plain TCP
// listener, as `rex server run --extra-listen http://…` serves.
func startTCPTestServer(t *testing.T) (addr string, stop func()) {
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
	cmd := exec.Command("rex", "server", "run",
		"--listen", "unix://"+filepath.Join(dir, "sock"),
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
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "sock")); err == nil {
			return fmt.Sprintf("127.0.0.1:%d", port), stop
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	t.Fatal("the test server did not come up")
	return "", nil
}

// isolateHosts points the settings directory at the test's, so the hosts
// the test saves touch nothing of the user's.
func isolateHosts(t *testing.T) {
	t.Helper()
	t.Setenv("GOREX_DIR", t.TempDir())
	saved := hosts
	hosts = hostStore{}
	t.Cleanup(func() { hosts = saved })
}

func TestEndpointLabel(t *testing.T) {
	cases := map[string]string{
		"rex.tailaab1cc.ts.net":         "rex",
		"rex.tailaab1cc.ts.net.":        "rex",
		"https://rex.tailaab1cc.ts.net": "rex",
		"http://192.168.1.20:7297":      "192",
		"studio.local:7297":             "studio",
		"studio":                        "studio",
		"":                              "server",
	}
	for in, want := range cases {
		if got := endpointLabel(in); got != want {
			t.Errorf("endpointLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostStore(t *testing.T) {
	isolateHosts(t)
	h, err := hosts.addHost("", "studio.tail1234.ts.net")
	if err != nil {
		t.Fatalf("addHost: %v", err)
	}
	if h.Label != "studio" {
		t.Errorf("label from endpoint: %q", h.Label)
	}
	// A duplicate endpoint names the host that has it.
	if _, err := hosts.addHost("other", "studio.tail1234.ts.net"); err == nil || !strings.Contains(err.Error(), "already saved") {
		t.Errorf("duplicate: %v", err)
	}
	// A bad endpoint names its mistake.
	if _, err := hosts.addHost("", "ssh://box"); err == nil {
		t.Error("ssh endpoints should fail validation for now")
	}
	if _, err := hosts.addHost("", ""); err == nil {
		t.Error("an empty address should fail")
	}
	// The file holds what was saved.
	loaded := hostStore{}
	if b, err := os.ReadFile(hostsPath()); err != nil {
		t.Fatalf("hosts.json: %v", err)
	} else if err := jsonUnmarshal(b, &loaded); err != nil {
		t.Fatalf("hosts.json: %v", err)
	}
	if len(loaded.Hosts) != 1 || loaded.Hosts[0].Endpoints[0] != "studio.tail1234.ts.net" {
		t.Errorf("hosts.json: %+v", loaded.Hosts)
	}
	// Edit renames and moves.
	if err := hosts.editHost(h, "workstation", "10.0.0.5:7297"); err != nil {
		t.Fatalf("editHost: %v", err)
	}
	if h.Label != "workstation" || h.Endpoints[0] != "10.0.0.5:7297" {
		t.Errorf("edited: %+v", h)
	}
	hosts.removeHost(h)
	if len(hosts.Hosts) != 0 {
		t.Errorf("removed: %+v", hosts.Hosts)
	}
}

// TestLastEndpointFirst checks the switcher tries the endpoint that last
// worked before the host's others.
func TestLastEndpointFirst(t *testing.T) {
	h := &Host{Endpoints: []string{"a", "b"}, LastEndpoint: "b"}
	if got := h.specs(); len(got) != 2 || got[0] != "b" || got[1] != "a" {
		t.Errorf("specs: %v", got)
	}
	h.LastEndpoint = ""
	if got := h.specs(); got[0] != "a" {
		t.Errorf("specs without last: %v", got)
	}
}

// TestSwitchServers connects an app to one throwaway server, then moves
// it to another, as the host switcher does: the session of the first
// stays where it is, the second's takes over the window.
func TestSwitchServers(t *testing.T) {
	isolateHosts(t)
	// The test starts its own servers; the machine's needs not run.
	addr1, stop1 := startTCPTestServer(t)
	defer stop1()
	addr2, stop2 := startTCPTestServer(t)
	defer stop2()

	registerFonts()
	hosts.Hosts = nil
	h1, err := hosts.addHost("server one", addr1)
	if err != nil {
		t.Fatalf("addHost one: %v", err)
	}
	h2, err := hosts.addHost("server two", addr2)
	if err != nil {
		t.Fatalf("addHost two: %v", err)
	}
	// The hosts' sessions are destroyed at the end: the app's blocks
	// belong to them.
	destroySessions := func(addr string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err := rex.Connect(ctx, "http://"+addr)
		if err != nil {
			return
		}
		defer c.Close()
		c.Bootstrap(ctx, []string{}...)
		if sessions, err := c.SessionList(ctx); err == nil {
			for _, s := range sessions {
				c.SessionDestroy(ctx, s.SessionID)
			}
		}
	}
	t.Cleanup(func() { destroySessions(addr1); destroySessions(addr2) })

	a := &App{}
	done := make(chan struct{})
	go func() {
		a.connectToServer(specForHost(h1), h1.ID)
		close(done)
	}()
	waitIdle(t, a, done, "the first server")
	if a.hostID != h1.ID {
		t.Fatalf("connected to host %q, want %q", a.hostID, h1.ID)
	}
	sid1 := a.sid
	if sid1 == "" {
		t.Fatal("no session on the first server")
	}
	if h1.LastFail != "" {
		t.Errorf("the first server is marked failed: %s", h1.LastFail)
	}

	// The window moves to the second server: another session, another
	// host, and the first one's client is gone.
	a.switchToServer(h2.ID)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		a.runPosted()
		if a.hostID == h2.ID && a.connecting == "" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if a.hostID != h2.ID {
		t.Fatalf("after the switch the host is %q; err %q", a.hostID, a.err)
	}
	if a.sid == "" || a.sid == sid1 {
		t.Errorf("the session did not change: %q", a.sid)
	}
	if a.err != "" {
		t.Errorf("err after the switch: %s", a.err)
	}
	// The second host remembers which endpoint answered, for the next try.
	if h2.LastEndpoint == "" {
		t.Error("the second host learned no endpoint")
	}

	// A server that does not answer leaves the window where it is.
	bad, err := hosts.addHost("away", "127.0.0.1:1")
	if err != nil {
		t.Fatalf("addHost away: %v", err)
	}
	a.switchToServer(bad.ID)
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		a.runPosted()
		if a.connecting == "" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if a.hostID != h2.ID {
		t.Errorf("the failed switch moved the host to %q", a.hostID)
	}
	if a.err == "" {
		t.Error("the failed switch says nothing")
	}
	if bad.LastFail == "" {
		t.Error("the failed host does not remember its failure")
	}
}

// waitIdle pumps the app's posted changes until the connection it is
// making settles.
func waitIdle(t *testing.T, a *App, done chan struct{}, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		a.runPosted()
		select {
		case <-done:
			return
		default:
		}
		if a.connecting == "" && a.client != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out connecting to %s; err %q", what, a.err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestHostSwitcherUI drives the host popover and the Add Server dialog:
// the dialog validates the address live, saves the host, and the
// switcher lists it.
func TestHostSwitcherUI(t *testing.T) {
	isolateHosts(t)
	registerFonts()
	a := &App{}
	tab := &Tab{ID: 1, app: a, Name: "shell"}
	n := &Node{Pane: &Pane{ID: 1, Tab: tab, info: rex.Info{Shell: "sh", Program: "sh", Idle: true}}}
	n.Pane.Node = n
	tab.Root, tab.Focus = n, n.Pane
	a.tabs = []*Tab{tab}
	tt := ui.NewTester(a.view, 1000, 620)
	tt.Frame()

	// The popover lists This computer and the Add Server row.
	a.hostOpen = true
	tt.Frame()
	if _, ok := tt.Find("This computer"); !ok {
		t.Fatalf("the switcher shows no local row; texts %q", tt.Texts())
	}
	if _, ok := tt.Find("Add Server…"); !ok {
		t.Fatalf("the switcher shows no add row; texts %q", tt.Texts())
	}

	// The dialog validates as it is typed: empty is a hint, a bad scheme
	// an error, a good address none.
	a.openHostDialog(nil)
	tt.Frame()
	if _, ok := tt.Find("Add Server"); !ok {
		t.Fatalf("no dialog; texts %q", tt.Texts())
	}
	// The name field has the focus; the tester types the host's name.
	tt.Type("studio")
	tt.Frame()
	if a.addHostLabel != "studio" {
		t.Fatalf("the name field took %q", a.addHostLabel)
	}
	// A bad address refuses the submit with its reason.
	a.addHostEndpoint = "ssh://box"
	a.submitHostDialog()
	if !a.addHostOpen || a.addHostErr == "" {
		t.Fatalf("ssh:// was not refused: open %v err %q", a.addHostOpen, a.addHostErr)
	}
	// A good one saves the host and closes the dialog.
	a.addHostErr = ""
	a.addHostEndpoint = "studio.tail1234.ts.net"
	a.submitHostDialog()
	if a.addHostOpen {
		t.Fatalf("the dialog stays open; err %q", a.addHostErr)
	}
	if len(hosts.Hosts) != 1 || hosts.Hosts[0].Label != "studio" {
		t.Fatalf("no host saved: %+v", hosts.Hosts)
	}
	// The switcher lists the new host, and the palette does too.
	tt.Frame()
	if _, ok := tt.Find("studio"); !ok {
		t.Errorf("the switcher does not list the host; texts %q", tt.Texts())
	}
	a.paletteQuery = "studio"
	found := false
	for _, it := range a.paletteItems() {
		if it.title == "Connect to studio" {
			found = true
		}
	}
	if !found {
		t.Error("the palette does not list the host")
	}
	// A duplicate is refused with its reason.
	a.openHostDialog(nil)
	a.addHostEndpoint = "studio.tail1234.ts.net"
	a.submitHostDialog()
	if !a.addHostOpen || !strings.Contains(a.addHostErr, "already saved") {
		t.Errorf("the duplicate is not refused: open %v err %q", a.addHostOpen, a.addHostErr)
	}
}
