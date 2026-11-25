package main

import (
	"context"
	"encoding/json"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"

	"gorex/internal/rex"
)

// Tab is a window of the session: panes in a tree of splits the server
// keeps.
type Tab struct {
	ID   int
	WID  string // the window's ID on the server
	Name string // its label, "" while the server names it
	app  *App
	Root *Node
	// Focus is the pane with the focus, and Zoom the pane that fills the
	// tab, when one does.
	Focus *Pane
	Zoom  *Pane
}

// Node is a pane, or a split of two nodes.
type Node struct {
	ID     int
	Pane   *Pane
	Parent *Node
	// SplitID is the server's name for the split. Vertical stacks A
	// above B; otherwise A is left of B. Ratio is A's share of the room.
	SplitID  string
	Vertical bool
	Ratio    float32
	A, B     *Node

	// The divider's drag asks the server for the border's new place at
	// most every while; sent is the last ratio it got.
	resizeAt time.Time
	sent     float64
	dirty    bool
}

// Pane is a terminal attached to a block of the session.
type Pane struct {
	ID      int
	Tab     *Tab
	Node    *Node
	BlockID string // the block's ID on the server
	term    *terminal.Terminal
	stream  *rex.Stream

	// info is what the server last said of the block's terminal.
	info rex.Info
	// title is the last title the program set.
	title string
	// lastData is when the terminal last printed, in Unix nanoseconds.
	lastData atomic.Int64
	// attention marks a pane that rang the bell or whose program finished
	// while it was out of sight; seen clears it.
	attention bool
	// bounds is where the pane's card was in the last frame.
	bounds ui.Rect
	closed bool
}

// App is the state of GoRex's window: a mirror of the session's view.
type App struct {
	win    atomic.Pointer[mygo.Window]
	client *rex.Client
	server string // the spec the connection opened, "" for the local socket
	status rex.ServerStatus
	sid    string // the session the window shows
	err    string

	tabs   []*Tab
	active int
	nextID int

	// focusReq asks the view to give the keyboard focus to a pane's
	// terminal.
	focusReq *Pane

	// The command palette, the host's popover and the tab being renamed.
	paletteOpen  bool
	paletteQuery string
	paletteSel   int
	hostOpen     bool
	renaming     *Tab
	renameText   string
	renameFocus  bool
	quitting     bool
	focusedWin   bool
	title        string

	// The session's view, by its revision: viewSeq is the applied one's,
	// viewMax the highest an event named. Block events that arrive while
	// a newer view is on its way wait in events.
	viewSeq   uint64
	viewMax   uint64
	viewDirty bool
	events    []rex.Event

	// posted are changes to make in the next frame, from terminals of a
	// view without a window, as in tests.
	postMu sync.Mutex
	posted []func()
	// services keeps redraw requests available to menu callbacks.
	services ui.Services
}

func (a *App) post(fn func()) {
	a.postMu.Lock()
	a.posted = append(a.posted, fn)
	a.postMu.Unlock()
}

// update runs fn on the thread the view builds on: the window's when
// there is one, the next frame otherwise.
func (a *App) update(fn func()) {
	if win := a.win.Load(); win != nil {
		win.Update(fn)
	} else {
		a.post(fn)
	}
}

// later changes the tree of tabs and panes once the frame building now
// is built, as closing what it is building would pull it from under it.
func (a *App) later(c *ui.Context, fn func()) {
	a.post(fn)
	c.Invalidate()
}

// laterService changes the app once a context menu's pick returns, which
// no frame wraps.
func (a *App) laterService(fn func()) {
	a.post(fn)
	a.services.Invalidate()
}

// runPosted makes the changes posted since the last frame.
func (a *App) runPosted() {
	a.postMu.Lock()
	fns := a.posted
	a.posted = nil
	a.postMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

func (a *App) id() int {
	a.nextID++
	return a.nextID
}

func (a *App) tab() *Tab {
	if a.active >= 0 && a.active < len(a.tabs) {
		return a.tabs[a.active]
	}
	return nil
}

// panes returns the panes of a node in order, left to right and top to
// bottom.
func (n *Node) panes() []*Pane {
	if n == nil {
		return nil
	}
	if n.Pane != nil {
		return []*Pane{n.Pane}
	}
	return append(n.A.panes(), n.B.panes()...)
}

func (t *Tab) panes() []*Pane { return t.Root.panes() }

// paneOf returns the pane showing block, in any tab.
func (a *App) paneOf(block string) *Pane {
	for _, t := range a.tabs {
		for _, p := range t.panes() {
			if p.BlockID == block {
				return p
			}
		}
	}
	return nil
}

// setFocus gives a pane the focus of its tab, telling the server, which
// says it back in the next view.
func (t *Tab) setFocus(p *Pane) {
	if p == nil || p == t.Focus {
		return
	}
	t.Focus = p
	p.attention = false
	if a := t.app; a != nil && a.sid != "" && a.client != nil {
		go a.client.FocusBlock(context.Background(), a.sid, p.BlockID)
	}
}

// newTab asks the server for a window with a shell in dir, after the
// active tab: the view that comes back shows it.
func (a *App) newTab(dir string) {
	if dir == "" {
		dir = a.currentDir()
	}
	var before string
	if t := a.tab(); t != nil && a.active+1 < len(a.tabs) {
		// The window that follows the active one, which the new window
		// goes before.
		before = a.tabs[a.active+1].WID
	}
	go func() {
		wid, _, err := a.client.NewWindow(context.Background(), a.sid, rex.TerminalOptions{Dir: dir})
		if err != nil {
			a.postErr(err)
			return
		}
		if before != "" {
			a.client.MoveWindow(context.Background(), a.sid, wid, before)
		}
	}()
}

// currentDir returns the directory new panes open in: the focused
// pane's.
func (a *App) currentDir() string {
	if t := a.tab(); t != nil && t.Focus != nil {
		if d := t.Focus.info.Dir; d != "" {
			return d
		}
	}
	home, _ := os.UserHomeDir()
	return home
}

// split asks the server for a pane beside the focused one, the new pane
// right of it or below it.
func (a *App) split(vertical bool) {
	t := a.tab()
	if t == nil || t.Focus == nil {
		return
	}
	old := t.Focus
	cols, rows := 80, 24
	if old.term != nil {
		c, r := old.term.Size()
		if vertical {
			rows = max(r/2, 2)
		} else {
			cols = max(c/2, 10)
		}
	}
	direction := "horizontal"
	if vertical {
		direction = "vertical"
	}
	dir := a.currentDir()
	go func() {
		_, err := a.client.NewSplit(context.Background(), a.sid, old.BlockID, direction, "after",
			rex.TerminalOptions{Dir: dir, Cols: cols, Rows: rows}, true)
		if err != nil {
			a.postErr(err)
		}
	}()
}

// closePane ends a pane's block: the view that comes back removes it.
func (a *App) closePane(p *Pane) {
	if p.closed {
		return
	}
	p.closed = true
	go func() {
		if err := a.client.CloseBlock(context.Background(), a.sid, p.BlockID); err != nil {
			a.postErr(err)
		}
	}()
}

// closeTab ends a window and the blocks of its panes.
func (a *App) closeTab(t *Tab) {
	for _, p := range t.panes() {
		p.closed = true
	}
	go func() {
		if err := a.client.CloseWindow(context.Background(), a.sid, t.WID); err != nil {
			a.postErr(err)
		}
	}()
}

func (a *App) selectTab(i int) {
	if i < 0 || i >= len(a.tabs) {
		return
	}
	a.active = i
	t := a.tabs[i]
	if t.Focus != nil {
		t.Focus.attention = false
	}
	a.focusReq = t.Focus
	go a.client.FocusWindow(context.Background(), a.sid, t.WID)
}

// moveFocus gives the focus to the pane next to the focused one, toward
// dx, dy.
func (a *App) moveFocus(dx, dy int) {
	t := a.tab()
	if t == nil || t.Focus == nil || t.Zoom != nil {
		return
	}
	from := t.Focus.bounds
	cx, cy := from.X+from.W/2, from.Y+from.H/2
	var best *Pane
	bestScore := float32(1e9)
	for _, p := range t.panes() {
		if p == t.Focus {
			continue
		}
		r := p.bounds
		var dist, overlap float32
		switch {
		case dx > 0:
			dist = r.X - (from.X + from.W)
			overlap = min(r.Y+r.H, from.Y+from.H) - max(r.Y, from.Y)
		case dx < 0:
			dist = from.X - (r.X + r.W)
			overlap = min(r.Y+r.H, from.Y+from.H) - max(r.Y, from.Y)
		case dy > 0:
			dist = r.Y - (from.Y + from.H)
			overlap = min(r.X+r.W, from.X+from.W) - max(r.X, from.X)
		default:
			dist = from.Y - (r.Y + r.H)
			overlap = min(r.X+r.W, from.X+from.W) - max(r.X, from.X)
		}
		if dist < -4 || overlap <= 0 {
			continue
		}
		// The nearest, then the one most in line with the focused pane's
		// center.
		px, py := r.X+r.W/2, r.Y+r.H/2
		off := px - cx
		if dx != 0 {
			off = py - cy
		}
		score := dist*4 + abs(off)
		if score < bestScore {
			best, bestScore = p, score
		}
	}
	if best != nil {
		t.setFocus(best)
		a.focusReq = best
	}
}

func abs(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}

// resizeFocused moves the nearest divider of the focused pane on the
// axis of dx, dy, step by step.
func (a *App) resizeFocused(dx, dy int) {
	t := a.tab()
	if t == nil || t.Focus == nil {
		return
	}
	vertical := dy != 0
	for n := t.Focus.Node; n.Parent != nil; n = n.Parent {
		p := n.Parent
		if p.Vertical != vertical {
			continue
		}
		step := float64(0.05)
		if dx < 0 || dy < 0 {
			step = -step
		}
		ratio := min(max(float64(p.Ratio)+step, 0.1), 0.9)
		p.Ratio = float32(ratio)
		go a.resizeSplit(p, ratio)
		return
	}
}

// resizeSplit asks the server to move a node's border: the blocks
// beside it name it.
func (a *App) resizeSplit(n *Node, ratio float64) {
	before, after := n.A.panes(), n.B.panes()
	if len(before) == 0 || len(after) == 0 {
		return
	}
	direction := "horizontal"
	if n.Vertical {
		direction = "vertical"
	}
	err := a.client.ResizePane(context.Background(), a.sid,
		before[len(before)-1].BlockID, after[0].BlockID, direction, ratio)
	if err != nil {
		a.postErr(err)
	}
}

// resizeSoon has the server move a divider being dragged, at most every
// 120 ms, and once more with the last ratio when the drag rests.
func (a *App) resizeSoon(n *Node) {
	const every = 120 * time.Millisecond
	if n.sent == 0 {
		n.sent = float64(n.Ratio)
	}
	now := time.Now()
	if now.Sub(n.resizeAt) >= every {
		n.resizeAt = now
		n.sent = float64(n.Ratio)
		go a.resizeSplit(n, n.sent)
		return
	}
	n.dirty = true
	time.AfterFunc(every-now.Sub(n.resizeAt), func() {
		a.update(func() {
			if !n.dirty {
				return
			}
			n.dirty = false
			ratio := float64(n.Ratio)
			if ratio != n.sent {
				n.resizeAt = time.Now()
				n.sent = ratio
				go a.resizeSplit(n, ratio)
			}
		})
	})
}

// equalize gives every split of the tab equal shares, by the panes on
// either side.
func (a *App) equalize() {
	t := a.tab()
	if t == nil {
		return
	}
	var walk func(n *Node) (cols, rows int)
	walk = func(n *Node) (int, int) {
		if n.Pane != nil {
			return 1, 1
		}
		ac, ar := walk(n.A)
		bc, br := walk(n.B)
		var ratio float64
		if n.Vertical {
			ratio = float64(ar) / float64(ar+br)
		} else {
			ratio = float64(ac) / float64(ac+bc)
		}
		ratio = min(max(ratio, 0.1), 0.9)
		n.Ratio = float32(ratio)
		go a.resizeSplit(n, ratio)
		if n.Vertical {
			return max(ac, bc), ar + br
		}
		return ac + bc, max(ar, br)
	}
	walk(t.Root)
}

func (a *App) toggleZoom() {
	t := a.tab()
	if t == nil || t.Focus == nil {
		return
	}
	if t.Zoom != nil {
		t.Zoom = nil
		go a.client.ZoomBlock(context.Background(), a.sid, t.Focus.BlockID)
	} else if len(t.panes()) > 1 {
		t.Zoom = t.Focus
		go a.client.ZoomBlock(context.Background(), a.sid, t.Focus.BlockID)
	}
	a.focusReq = t.Focus
}

// label returns how a pane names itself: its program, or the title the
// program set, and the directory it works in.
func (p *Pane) label() (name, detail string) {
	in := p.info
	prog := programOf(in.Program)
	dir := shortDir(in.Dir)
	if in.Program == "" {
		return in.Shell, dir
	}
	if in.Idle || prog.Shell {
		name := in.Program
		if name == "" {
			name = in.Shell
		}
		return name, dir
	}
	if t := strings.TrimSpace(p.title); t != "" && !titleOfShell(t, in) {
		return t, ""
	}
	name = prog.Name
	if name == "ssh" || in.Program == "ssh" {
		if host := sshTarget(in.Argv0, p.title); host != "" {
			return "SSH", host
		}
	}
	return name, dir
}

// sshTarget finds the host ssh connects to in its argv0 or its title,
// since the server never hands a process's arguments out.
func sshTarget(argv0, title string) string {
	if strings.HasPrefix(argv0, "ssh ") {
		for _, w := range strings.Fields(argv0)[1:] {
			if !strings.HasPrefix(w, "-") {
				return w
			}
		}
	}
	if strings.HasPrefix(title, "ssh ") {
		return strings.TrimPrefix(title, "ssh ")
	}
	return ""
}

// titleOfShell tells a title a shell set, naming the program or the
// directory, from one a program set of its own.
func titleOfShell(title string, in rex.Info) bool {
	l := strings.ToLower(title)
	for _, w := range []string{strings.ToLower(in.Program), strings.ToLower(in.Shell)} {
		if w != "" && (strings.HasPrefix(l, w) || strings.Contains(l, `\`+w+".exe")) {
			return true
		}
	}
	if home, _ := os.UserHomeDir(); strings.Contains(title, "~") || (home != "" && strings.Contains(title, home)) {
		return true
	}
	return strings.Contains(title, "@") && strings.Contains(title, ":")
}

// shortDir shows a directory as shells do, from ~, and only its last
// parts when it is deep.
func shortDir(d string) string {
	if d == "" {
		return ""
	}
	sep := string(filepath.Separator)
	if home, _ := os.UserHomeDir(); home != "" {
		if samePath(d, home) {
			return "~"
		}
		if len(d) > len(home) && samePath(d[:len(home)], home) && d[len(home)] == filepath.Separator {
			d = "~" + d[len(home):]
		}
	}
	parts := strings.Split(d, sep)
	if len(parts) > 5 || len(d) > 48 && len(parts) > 3 {
		return "…" + sep + strings.Join(parts[len(parts)-2:], sep)
	}
	return d
}

// samePath reports whether two paths are the same, without case on
// Windows.
func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// tabLabel names a tab: its name, or its focused pane's.
func (t *Tab) label() (name, detail string) {
	if t.Name != "" {
		return t.Name, ""
	}
	if t.Focus == nil {
		return "Shell", ""
	}
	return t.Focus.label()
}

// attach makes the pane's terminal, attached to its block's stream.
func (a *App) attach(p *Pane, cols, rows int) {
	p.stream = a.client.Stream(a.sid, p.BlockID, cols, rows)
	p.stream.OnData = func(n int) { p.lastData.Store(time.Now().UnixNano()) }
	update := func(fn func()) { a.update(fn) }
	p.stream.OnResize = func(cols, rows int) {
		update(func() {
			if p.term != nil {
				c, r := p.term.Size()
				if c != cols || r != rows {
					p.term.Resize(cols, rows)
				}
			}
		})
	}
	term, err := terminal.New(terminal.Options{
		Conn:        p.stream,
		Font:        termFont,
		Theme:       lightTerm,
		DarkTheme:   darkTerm,
		Transparent: true,
		OnTitle: func(title string) {
			update(func() { p.title = title })
		},
		OnBell: func() {
			update(func() { a.ring(p) })
		},
		OnNotify: func(title, body string) {
			if title == "" {
				title = "GoRex"
			}
			mygo.NewNotification(mygo.NotificationOptions{Title: title, Body: body}).Show()
		},
	})
	if err != nil {
		log.Printf("terminal: %v", err)
		p.stream.Close()
		p.stream = nil
		a.err = err.Error()
		return
	}
	// Until a view shows it, the terminal has the block's size, so that
	// attaching changes nothing in it.
	term.Resize(cols, rows)
	p.term = term
}

func (a *App) ring(p *Pane) {
	if t := a.tab(); t == nil || t.Focus != p || p.Tab != t || !a.focusedWin {
		p.attention = true
	}
}

// postErr reports a failed call on the banner.
func (a *App) postErr(err error) {
	a.post(func() { a.err = err.Error() })
}

// detach ends the pane's terminal and stream without closing the block.
func (p *Pane) detach() {
	if p.term != nil {
		p.term.Close()
		p.term = nil
	}
	if p.stream != nil {
		p.stream.Close()
		p.stream = nil
	}
}

// reattach replaces a pane whose stream ended, as after a reconnect.
func (a *App) reattach(p *Pane) {
	if p.stream != nil {
		p.detach()
		cols, rows := p.info.Cols, p.info.Rows
		if cols <= 0 || rows <= 0 {
			cols, rows = 80, 24
		}
		a.attach(p, cols, rows)
	}
}

// applyView takes the session's view: it builds the tabs and panes the
// server shows, reusing the panes of blocks it already has.
func (a *App) applyView(v *rex.SessionView) {
	if v == nil {
		return
	}
	byBlock := map[string]*Pane{}
	for _, t := range a.tabs {
		for _, p := range t.panes() {
			byBlock[p.BlockID] = p
		}
	}
	var build func(vn *rex.ViewNode, t *Tab, parent *Node) *Node
	build = func(vn *rex.ViewNode, t *Tab, parent *Node) *Node {
		n := &Node{ID: a.id(), Parent: parent}
		if vn == nil {
			return n
		}
		if vn.Split == nil {
			block := vn.BlockID
			p := byBlock[block]
			if p == nil {
				p = &Pane{ID: a.id(), BlockID: block}
				// Owning the block lets this client size and write its
				// terminal.
				go a.client.OwnBlock(context.Background(), a.sid, "take", block)
				go a.fetchInfo(p)
				a.attach(p, 80, 24)
			} else {
				delete(byBlock, block)
			}
			p.Tab, p.Node, p.closed = t, n, false
			n.Pane = p
			return n
		}
		n.SplitID = vn.Split.SplitID
		n.Vertical = vn.Split.Direction == "vertical"
		n.Ratio = float32(min(max(vn.Split.Ratio, 0.05), 0.95))
		n.A = build(vn.Split.Before, t, n)
		n.B = build(vn.Split.After, t, n)
		return n
	}
	var tabs []*Tab
	for _, vw := range v.Windows {
		var t *Tab
		var oldFocus *Pane
		for _, ot := range a.tabs {
			if ot.WID == vw.WindowID {
				t, oldFocus = ot, ot.Focus
				break
			}
		}
		if t == nil {
			t = &Tab{ID: a.id(), WID: vw.WindowID}
		}
		t.app = a
		t.Name = windowName(vw.Label)
		t.Root = nil
		t.Focus, t.Zoom = nil, nil
		for _, layer := range vw.Layers {
			if layer.Kind == "tiled" {
				t.Root = build(layer.Layout, t, nil)
			}
		}
		if t.Root == nil {
			t.Root = &Node{ID: a.id()}
		}
		if vw.FocusedBlockID != "" {
			for _, p := range t.panes() {
				if p.BlockID == vw.FocusedBlockID {
					t.Focus = p
				}
			}
		}
		if t.Focus == nil {
			if ps := t.panes(); len(ps) > 0 {
				t.Focus = ps[0]
			}
		}
		if vw.ZoomedBlockID != "" {
			for _, p := range t.panes() {
				if p.BlockID == vw.ZoomedBlockID {
					t.Zoom = p
				}
			}
		}
		// The keyboard focus follows a focus the server moved, such as
		// another client's.
		if t.Focus != nil && t.Focus != oldFocus {
			a.focusReq = t.Focus
		}
		tabs = append(tabs, t)
	}
	// The panes of blocks the view no longer shows: their stream ends,
	// their terminals close.
	for _, p := range byBlock {
		p.detach()
	}
	a.tabs = tabs
	// The active window is the active tab.
	for i, vw := range v.Windows {
		if vw.WindowID == v.ActiveWindowID && i < len(tabs) {
			a.active = i
		}
	}
	if a.renaming != nil && !slices.Contains(tabs, a.renaming) {
		a.renaming, a.renameFocus = nil, false
	}
	if a.active >= len(tabs) {
		a.active = max(len(tabs)-1, 0)
	}
	if len(tabs) == 0 {
		if !a.quitting {
			if win := a.win.Load(); win != nil {
				win.Close()
			}
		}
	}
}

// windowName tells a window's label from the default the server makes:
// "Window 89abcdef" names nothing the user chose.
func windowName(label string) string {
	if strings.HasPrefix(label, "Window ") {
		return ""
	}
	return label
}

// fetchInfo fills a pane's info from the block's terminal: what runs in
// it, its title and directory.
func (a *App) fetchInfo(p *Pane) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var in rex.Info
	if pi, err := a.client.Process(ctx, a.sid, p.BlockID); err == nil {
		in.Fill(nil, pi)
	}
	if title, err := a.client.Title(ctx, a.sid, p.BlockID); err == nil {
		in.Title = title
	}
	if pwd, err := a.client.Pwd(ctx, a.sid, p.BlockID); err == nil {
		if d := pwdPath(pwd); d != "" {
			in.Dir = d
		}
	}
	if in.Shell == "" {
		in.Shell = "shell"
	}
	cols, rows, _ := a.client.Size(ctx, a.sid, p.BlockID)
	in.Cols, in.Rows = cols, rows
	a.update(func() {
		p.info = in
		if p.term != nil && cols > 0 && rows > 0 {
			c, r := p.term.Size()
			if c != cols || r != rows {
				p.term.Resize(cols, rows)
			}
		}
	})
}

// pwdPath turns a reported working directory into a path: a file:// URL
// when the program used OSC 7, a plain path otherwise.
func pwdPath(pwd string) string {
	if strings.HasPrefix(pwd, "file://") {
		u := strings.TrimPrefix(pwd, "file://")
		if i := strings.Index(u, "/"); i >= 0 {
			u = u[i:]
		}
		if r, err := url.PathUnescape(u); err == nil {
			return r
		}
		return u
	}
	return pwd
}

// join picks the session the window shows — the first the server has,
// or a new one — attaches to it and takes its blocks. It touches no app
// state, so it may run off the main thread; the caller applies what it
// returns.
func (a *App) join(ctx context.Context, client *rex.Client) (b *rex.Bootstrap, sid string, v *rex.SessionView, err error) {
	b, err = client.Bootstrap(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	if b.SelectedSession != nil {
		sid = b.SelectedSession.Session.SessionID
		v = &b.SelectedSession.View
	}
	if sid == "" {
		for _, s := range b.AttachAll.Sessions {
			sid = s.SessionID
			break
		}
	}
	if sid == "" {
		home, _ := os.UserHomeDir()
		if sid, err = client.SessionCreateShell(ctx, "gorex", home); err != nil {
			return nil, "", nil, err
		}
	}
	if v == nil {
		if v, _, err = client.Attach(ctx, sid); err != nil {
			return nil, "", nil, err
		}
	}
	// Owning the session's blocks lets this client size and write their
	// terminals.
	client.OwnBlock(ctx, sid, "take")
	return b, sid, v, nil
}

// watch takes the server's events until the connection ends, then tries
// to resume the client before giving up.
func (a *App) watch(win *mygo.Window, client *rex.Client) {
	<-client.Closed()
	if a.quitting {
		return
	}
	a.update(func() {
		if a.client != client || a.quitting {
			return // a newer connection replaced this one
		}
		a.err = "Lost the connection to the Rex server; reconnecting…"
	})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	var nc *rex.Client
	var err error
	for range 8 {
		if a.quitting {
			return
		}
		if nc, err = client.Reconnect(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	if err != nil {
		a.update(func() {
			if a.client == client && !a.quitting {
				a.err = "Lost the connection to the Rex server."
			}
		})
		return
	}
	nc.GhosttyPath = client.GhosttyPath
	// Events reach the app as soon as the new connection's bootstrap
	// lets them flow.
	a.bindEvents(nc)
	b, sid, v, jerr := a.join(ctx, nc)
	if jerr != nil {
		nc.Close()
		a.update(func() {
			if a.client == client && !a.quitting {
				a.err = "The Rex server resumed the connection but not the session: " + jerr.Error()
			}
		})
		return
	}
	a.update(func() {
		if a.client != client {
			nc.Close()
			return
		}
		a.client = nc
		a.status = b.ServerStatus
		a.sid = sid
		a.err = ""
		a.viewSeq = max(a.viewSeq, v.Revision)
		a.viewMax = a.viewSeq
		a.viewDirty = false
		a.events = nil
		a.applyView(v)
		// Streams of the dropped connection ended; each pane opens a new
		// one, which restores its terminal from a fresh snapshot.
		for _, t := range a.tabs {
			for _, p := range t.panes() {
				a.reattach(p)
			}
		}
		go a.watch(win, nc)
	})
}

// bindEvents makes the client's events reach the app on the view's
// thread.
func (a *App) bindEvents(client *rex.Client) {
	client.OnEvent = func(ev rex.Event) {
		a.update(func() { a.onEvent(ev) })
	}
}

// onEvent applies one event, on the main thread.
func (a *App) onEvent(ev rex.Event) {
	if a.sid != "" && ev.SessionID != "" && ev.SessionID != a.sid {
		return
	}
	switch ev.Type {
	case "session_destroyed":
		a.sid = ""
		a.err = "The session ended."
	case "session_view_changed":
		a.viewMax = max(a.viewMax, ev.Revision)
		if ev.Revision > a.viewSeq {
			a.viewDirty = true
			go a.refresh()
		}
	case "block_event":
		if a.viewDirty {
			// A view is coming; the event may describe what it already
			// shows, so it waits to be seen after it.
			if len(a.events) < 64 {
				a.events = append(a.events, ev)
			}
			return
		}
		a.onBlockEvent(ev)
	}
}

// refresh fetches the view until no change is left, then the queued
// block events, on the main thread.
func (a *App) refresh() {
	if a.sid == "" || a.client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	v, _, err := a.client.View(ctx, a.sid)
	cancel()
	if err != nil {
		return
	}
	a.update(func() {
		if v.Revision > a.viewSeq {
			a.viewSeq = v.Revision
			a.applyView(v)
		}
		// The view covers its own revision; a newer event asks again.
		a.viewDirty = a.viewMax > a.viewSeq
		events := a.events
		a.events = nil
		for _, ev := range events {
			a.onBlockEvent(ev)
		}
		if a.viewDirty {
			go a.refresh()
		}
	})
}

// onBlockEvent takes a block's event: what runs in it, where it works,
// its title, bell and exit.
func (a *App) onBlockEvent(ev rex.Event) {
	if ev.Creator != "com.superlogical.terminal" {
		return
	}
	p := a.paneOf(ev.BlockID)
	if p == nil {
		return
	}
	switch ev.Name {
	case "process_changed":
		var pc rex.ProcessChanged
		if json.Unmarshal(ev.Payload, &pc) == nil {
			was := p.info
			p.info.Fill(&pc, nil)
			a.finished(p, was)
		}
	case "title_changed":
		var pl struct {
			Title string `json:"title"`
		}
		json.Unmarshal(ev.Payload, &pl)
		p.info.Title, p.title = pl.Title, pl.Title
	case "pwd_changed":
		var pl struct {
			Pwd string `json:"pwd"`
		}
		if json.Unmarshal(ev.Payload, &pl) == nil {
			p.info.Dir = pwdPath(pl.Pwd)
		}
	case "child_exited":
		var pl struct {
			ExitCode int `json:"exit_code"`
		}
		json.Unmarshal(ev.Payload, &pl)
		p.info.Exited, p.info.ExitCode = true, pl.ExitCode
	case "bell":
		a.ring(p)
	case "desktop_notification":
		var pl struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		if json.Unmarshal(ev.Payload, &pl) == nil {
			if pl.Title == "" {
				pl.Title = "GoRex"
			}
			mygo.NewNotification(mygo.NotificationOptions{Title: pl.Title, Body: pl.Body}).Show()
		}
	case "size_changed":
		var pl struct {
			Columns int `json:"columns"`
			Rows    int `json:"rows"`
		}
		if json.Unmarshal(ev.Payload, &pl) == nil {
			p.info.Cols, p.info.Rows = pl.Columns, pl.Rows
		}
	}
}

// finished notes a program that ended out of sight, asking for
// attention and sending a notification.
func (a *App) finished(p *Pane, was rex.Info) {
	in := p.info
	if !(!was.Idle && was.Program != "" && in.Idle && was.Program != in.Program) {
		return
	}
	seen := p.Tab == a.tab() && p.Tab.Focus == p && a.focusedWin
	if !seen {
		p.attention = true
	}
	if !a.focusedWin && time.Since(p.lastInput()) > 8*time.Second {
		prog := programOf(was.Program)
		n := mygo.NewNotification(mygo.NotificationOptions{
			Title: prog.Name + " finished",
			Body:  "in " + shortDir(in.Dir),
		})
		n.Show()
	}
}

// lastInput is when the pane last typed, which its stream's writes
// track; never, if it has none yet.
func (p *Pane) lastInput() time.Time {
	if p.stream == nil {
		return time.Time{}
	}
	return time.Unix(0, p.stream.LastWrite())
}
