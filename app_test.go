package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"

	"gorex/internal/rex"
)

// testShell is the shell of the tests: its prompt ends with prompt, and
// typing cd then run, each on a line of its own — cmd and PowerShell
// share no command separator — runs program for a while in dir.
var testShell = func() (s struct{ path, prompt, run, program, dir string }) {
	if runtime.GOOS == "windows" {
		s.path, s.prompt, s.run, s.program, s.dir = "cmd.exe", ">", "ping -n 4 127.0.0.1", "PING", os.Getenv("SystemRoot")
		return
	}
	s.path, s.prompt, s.run, s.program, s.dir = "/bin/sh", "$", "sleep 3", "sleep", "/usr/bin"
	return
}()

// liveServer skips the test unless a Rex server answers: it stat()s the
// Unix socket, while Windows's pipe answers only to a dial.
func liveServer(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		if _, err := os.Stat(rex.SocketPath()); err != nil {
			t.Skipf("no Rex server at %s; start one with `rex server start`", rex.SocketPath())
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := rex.Connect(ctx, "")
	if err != nil {
		t.Skipf("no Rex server answers: %v; start one with `rex server start`", err)
	}
	c.Close()
}

// newTestClient connects to the running server, with the library the
// streams decode snapshots with.
func newTestClient(t *testing.T) *rex.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	client, err := rex.Connect(ctx, "")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	client.GhosttyPath, _ = terminal.LibraryPath()
	t.Cleanup(func() { client.Close() })
	return client
}

// newTestSession makes a session for the test, destroyed at its end.
// The client attaches right away: calls on the session need it, and the
// cleanup's destroy does too.
func newTestSession(t *testing.T, client *rex.Client) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sid, err := client.SessionCreate(ctx, "gorex-test")
	if err != nil {
		t.Fatalf("session.create: %v", err)
	}
	if _, _, err := client.Attach(ctx, sid); err != nil {
		t.Fatalf("session.attach: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		client.SessionDestroy(ctx, sid)
	})
	return sid
}

// attachSession attaches the client to the session, which calls that
// change it need.
func attachSession(t *testing.T, client *rex.Client, sid string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := client.Attach(ctx, sid); err != nil {
		t.Fatalf("session.attach: %v", err)
	}
}

// newTestApp joins an app to the test's session: attached, its view
// applied, events bound, and a window-less view a tester drives.
func newTestApp(t *testing.T) (*App, *ui.Tester) {
	t.Helper()
	liveServer(t)
	registerFonts()
	client := newTestClient(t)
	sid := newTestSession(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v, _, err := client.View(ctx, sid)
	if err != nil {
		t.Fatalf("session.view: %v", err)
	}
	a := &App{client: client, sid: sid}
	a.bindEvents(client)
	a.viewSeq = v.Revision
	a.applyView(v)
	tt := ui.NewTester(a.view, 1000, 620)
	return a, tt
}

// waitFor runs frames until cond holds, or fails after a few seconds.
func waitFor(t *testing.T, tt *ui.Tester, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; texts %q", what, tt.Texts())
		}
		time.Sleep(25 * time.Millisecond)
		tt.Frame()
	}
}

func TestPanesAndTabs(t *testing.T) {
	a, tt := newTestApp(t)
	// The session starts with no window; the app asks for one, as open
	// does.
	a.newTab(os.TempDir())
	waitFor(t, tt, "a window", func() bool { return len(a.tabs) == 1 && a.tab().Focus != nil })
	tab := a.tab()
	p := tab.Focus
	waitFor(t, tt, "the shell", func() bool {
		return p.term != nil && strings.Contains(p.term.Text(), testShell.prompt)
	})

	// The command palette splits the pane.
	a.openPalette()
	tt.Frame()
	tt.Type("split right")
	tt.Key(0, ui.KeyEnter)
	waitFor(t, tt, "two panes", func() bool { return len(tab.panes()) == 2 })
	if a.paletteOpen {
		t.Error("the palette stays open")
	}
	right := tab.Focus
	if right == p {
		t.Fatal("the new pane does not have the focus")
	}
	waitFor(t, tt, "the second shell", func() bool {
		return right.term != nil && strings.Contains(right.term.Text(), testShell.prompt)
	})

	// Typing reaches the shell, and the header follows its program.
	a.focusReq = p
	tab.setFocus(p)
	tt.Frame()
	p.term.Send([]byte("cd " + testShell.dir + "\r"))
	p.term.Send([]byte(testShell.run + "\r"))
	deadline := time.Now().Add(10 * time.Second)
	for {
		name, detail := p.label()
		if strings.EqualFold(name, testShell.program) && samePath(detail, shortDir(testShell.dir)) {
			break
		}
		if time.Now().After(deadline) {
			text := ""
			if p.term != nil {
				text = p.term.Text()
			}
			t.Fatalf("timed out waiting for the header; info %+v; terminal shows %q", p.info, text)
		}
		time.Sleep(25 * time.Millisecond)
		tt.Frame()
	}

	// A second tab, renamed, then closed with its pane.
	a.newTab(os.TempDir())
	waitFor(t, tt, "the second tab", func() bool { return len(a.tabs) == 2 })
	if a.active != 1 {
		t.Fatalf("active tab %d", a.active)
	}
	a.startRename()
	tt.Frame()
	tt.Type("logs")
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	if a.tabs[1].Name != "logs" {
		t.Errorf("tab named %q", a.tabs[1].Name)
	}
	a.closeFocused()
	waitFor(t, tt, "one tab", func() bool { return len(a.tabs) == 1 })

	// A shell that exits ends its block: the pane closes with it.
	a.selectTab(0)
	tab = a.tab()
	tab.setFocus(tab.panes()[1])
	tab.Focus.term.Send([]byte("exit\r"))
	waitFor(t, tt, "the pane to close", func() bool { return len(tab.panes()) == 1 })
}

func TestSplitAndZoom(t *testing.T) {
	a, tt := newTestApp(t)
	a.newTab(os.TempDir())
	waitFor(t, tt, "a window", func() bool { return len(a.tabs) == 1 && a.tab().Focus != nil })
	tab := a.tab()
	a.split(false)
	waitFor(t, tt, "two panes", func() bool { return len(tab.panes()) == 2 })
	a.toggleZoom()
	waitFor(t, tt, "the zoom", func() bool { return tab.Zoom != nil })
	a.toggleZoom()
	a.closeTab(tab)
	waitFor(t, tt, "no tabs", func() bool { return len(a.tabs) == 0 })
}

// TestReattach checks that another client sees the same session's view,
// and the app's panes the test session's blocks.
func TestReattach(t *testing.T) {
	liveServer(t)
	client := newTestClient(t)
	sid := newTestSession(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wid, bid, err := client.NewWindow(ctx, sid, rex.TerminalOptions{Dir: os.TempDir()})
	if err != nil {
		t.Fatalf("new_window: %v", err)
	}
	// A second connection reads the session's view and finds the window.
	other := newTestClient(t)
	attachSession(t, other, sid)
	v, _, err := other.View(ctx, sid)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if len(v.Windows) != 1 || v.Windows[0].WindowID != wid {
		t.Fatalf("view shows %+v", v.Windows)
	}
	var found bool
	for _, layer := range v.Windows[0].Layers {
		for _, b := range layer.Blocks {
			if b.BlockID == bid {
				found = true
			}
		}
	}
	var walk func(n *rex.ViewNode)
	walk = func(n *rex.ViewNode) {
		if n == nil {
			return
		}
		if n.BlockID == bid {
			found = true
		}
		if n.Split != nil {
			walk(n.Split.Before)
			walk(n.Split.After)
		}
	}
	for _, layer := range v.Windows[0].Layers {
		walk(layer.Layout)
	}
	if !found {
		t.Errorf("block %s not in the view", bid)
	}
}

func TestLabels(t *testing.T) {
	home, _ := os.UserHomeDir()
	site, short := filepath.Join(home, "Sites", "rex-snake"), filepath.Join("~", "Sites", "rex-snake")
	p := &Pane{info: rex.Info{Shell: "zsh", Program: "zsh", Idle: true, Dir: site}}
	if n, d := p.label(); n != "zsh" || d != short {
		t.Errorf("shell label %q %q", n, d)
	}
	p.info = rex.Info{Shell: "fish", Program: "lazygit", Dir: site}
	p.title = "lazygit ~/Sites/rex-snake"
	if n, d := p.label(); n != "Git Changes" || d != short {
		t.Errorf("lazygit label %q %q", n, d)
	}
	p.info.Program = "codex"
	p.title = "Stress-test Snake demo | rex-snake"
	if n, d := p.label(); n != p.title || d != "" {
		t.Errorf("codex label %q %q", n, d)
	}
	p.info, p.title = rex.Info{Shell: "zsh", Program: "ssh", Argv0: "ssh box.local"}, ""
	if n, d := p.label(); n != "SSH" || d != "box.local" {
		t.Errorf("ssh label %q %q", n, d)
	}
	// Windows's consoles name the shell in their title as a program runs.
	p.info, p.title = rex.Info{Shell: "cmd", Program: "node", Dir: site}, `C:\WINDOWS\system32\cmd.exe - node app.js`
	if n, d := p.label(); n != "Node" || d != short {
		t.Errorf("node label %q %q", n, d)
	}
	if d := shortDir(filepath.FromSlash("/private/tmp/a/b/c/d/e")); d != filepath.FromSlash("…/d/e") {
		t.Errorf("short dir %q", d)
	}
}

// TestCloseButtons presses and releases the close buttons that show while
// the pointer is over a tab or a pane, with a frame between: pressing one
// must not hide it.
func TestCloseButtons(t *testing.T) {
	a, tt := newTestApp(t)
	a.newTab(os.TempDir())
	waitFor(t, tt, "a window", func() bool { return len(a.tabs) == 1 })
	a.newTab(os.TempDir())
	waitFor(t, tt, "two tabs", func() bool { return len(a.tabs) == 2 })
	tt.Frame()
	track, ok := tt.Find("Tabs")
	if !ok {
		t.Fatal("no tab bar")
	}
	first := a.tabs[0]
	// The pointer over the first tab shows its close button.
	tt.Move(track.X+40, track.Y+track.H/2)
	tt.Frame()
	x, ok := tt.Find("Close Tab")
	if !ok {
		t.Fatalf("no close button over the tab; texts %q", tt.Texts())
	}
	tt.Press(x.X+x.W/2, x.Y+x.H/2)
	tt.Frame()
	tt.Release(x.X+x.W/2, x.Y+x.H/2)
	waitFor(t, tt, "one tab", func() bool { return len(a.tabs) == 1 && !slices.Contains(a.tabs, first) })

	// A pane without the focus shows its buttons under the pointer.
	a.split(false)
	waitFor(t, tt, "two panes", func() bool { return len(a.tab().panes()) == 2 })
	tt.Frame()
	tab := a.tab()
	left := tab.panes()[0]
	if tab.Focus == left {
		t.Fatal("the left pane has the focus")
	}
	b := left.bounds
	tt.Move(b.X+60, b.Y+headerH/2)
	tt.Frame()
	cx, cy := b.X+b.W-8-13, b.Y+headerH/2 // the last button of the header
	tt.Press(cx, cy)
	tt.Frame()
	tt.Release(cx, cy)
	waitFor(t, tt, "one pane", func() bool {
		ps := tab.panes()
		return len(ps) == 1 && ps[0] != left
	})
}

// TestPaletteScrolls checks that the wheel scrolls a palette longer than
// it shows, and that the keys bring the selection back into view.
func TestPaletteScrolls(t *testing.T) {
	registerFonts()
	a := &App{}
	tab := &Tab{ID: 1, app: a}
	for i := range 20 {
		n := &Node{Pane: &Pane{ID: i, Tab: tab, info: rex.Info{Shell: fmt.Sprintf("shell%02d", i)}}}
		n.Pane.Node = n
		if tab.Root == nil {
			tab.Root = n
		} else {
			tab.Root = &Node{A: tab.Root, B: n, Ratio: 0.5}
		}
	}
	a.tabs = []*Tab{tab}
	tt := ui.NewTester(func(c *ui.Context) { a.palette(c, colorsOf(c)) }, 1000, 620)
	a.openPalette()
	tt.Frame()
	// A row out of sight finds an empty box.
	top, _ := tt.Find("shell00")
	row10, _ := tt.Find("shell10")
	tt.Move(top.X, top.Y+100)
	tt.Scroll(top.X, top.Y+100, 0, 300)
	tt.Frame()
	if r, _ := tt.Find("shell10"); r.Y != row10.Y-300 {
		t.Errorf("scrolled 300: a row moved from %v to %v", row10.Y, r.Y)
	}
	if r, _ := tt.Find("shell00"); r.H != 0 {
		t.Errorf("scrolled 300: the first row still shows, at %v", r.Y)
	}
	tt.Key(0, ui.KeyDown)
	tt.Frame()
	if r, _ := tt.Find("shell01"); r.H == 0 {
		t.Error("the keys chose a row out of sight")
	}
}
