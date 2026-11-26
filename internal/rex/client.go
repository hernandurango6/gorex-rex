package rex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Client is the app's control connection to a Rex server.
type Client struct {
	Ep Endpoint

	// GhosttyPath is the libghostty-vt the data streams decode snapshots
	// with; the app's terminal library names it. Empty tries
	// MYGO_GHOSTTY_VT.
	GhosttyPath string

	ws  *websocket.Conn
	ctx context.Context
	end context.CancelFunc

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan response
	err     error
	closed  chan struct{}
	token   atomic.Value // reconnect token of the last welcome
	id      string
	seq     atomic.Uint64

	// OnEvent is called for every event the server sends, in order, from
	// the reading goroutine.
	OnEvent func(Event)
}

// Connect opens the control connection to the server spec names, as
// REX_SERVER spells it or the caller passes: "" is this machine's
// socket, or on Windows its named pipe. A bare host or host:port is
// probed the way rex's -S does —
// HTTPS, then HTTPS on Rex's tailnet port, then plain TCP on it — and
// every connection to the same server must spell its tailnet name for
// the TLS certificate to verify. The client doesn't start a server:
// Rex's own does that, and `rex server start` brings it up.
func Connect(ctx context.Context, serverSpec string) (*Client, error) {
	return ConnectAny(ctx, []string{serverSpec})
}

// ConnectAny opens the control connection trying each endpoint spec in
// turn, as a host's endpoints list them, and returns the client of the
// first that answers. Each spec parses as Connect's does; a probe's
// candidates count as the spec's one try. Empty means this machine's
// socket.
func ConnectAny(ctx context.Context, specs []string) (*Client, error) {
	if len(specs) == 0 {
		specs = []string{""}
	}
	var last error
	for _, s := range specs {
		eps, _, err := spec(s)
		if err != nil {
			last = err
			continue
		}
		for _, ep := range eps {
			pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			c, err := connect(pctx, ep, "")
			cancel()
			if err == nil {
				return c, nil
			}
			last = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
	}
	return nil, last
}

// Reconnect opens a new control connection that continues this one after
// it dropped: the server kept its client, its attached sessions and its
// owned blocks for a while. Events since the drop are lost, so the view
// must be read again.
func (c *Client) Reconnect(ctx context.Context) (*Client, error) {
	token, _ := c.token.Load().(string)
	return connect(ctx, c.Ep, token)
}

// connect opens the control connection, resuming the client of token
// when it names one.
func connect(ctx context.Context, ep Endpoint, token string) (*Client, error) {
	ws, err := dialWS(ctx, ep, "/api/control", "rex.control.v1")
	if err != nil {
		return nil, fmt.Errorf("rex: connecting to %s: %w", ep, err)
	}
	ws.SetReadLimit(16 << 20)
	c := &Client{Ep: ep, ws: ws, pending: map[string]chan response{}, closed: make(chan struct{})}
	c.ctx, c.end = context.WithCancel(context.Background())
	if token != "" {
		c.token.Store(token)
	}
	h := hello{
		Type:           "hello",
		APIRevision:    APIRevision,
		APIMinRevision: APIRevision,
		ReconnectToken: token,
		Client:         clientHello{Kind: "app", Name: "gorex", Version: "0.2.0", Label: "GoRex"},
	}
	b, _ := json.Marshal(h)
	wctx, wc := context.WithTimeout(ctx, 5*time.Second)
	err = ws.Write(wctx, websocket.MessageText, b)
	wc()
	if err != nil {
		ws.Close(websocket.StatusInternalError, "")
		return nil, err
	}
	rctx, rc := context.WithTimeout(ctx, 5*time.Second)
	_, data, err := ws.Read(rctx)
	rc()
	if err != nil {
		ws.Close(websocket.StatusInternalError, "")
		return nil, fmt.Errorf("rex: waiting for the server's welcome: %w", err)
	}
	var w welcome
	if json.Unmarshal(data, &w) != nil || w.Type != "welcome" {
		ws.Close(websocket.StatusProtocolError, "")
		return nil, fmt.Errorf("rex: the server's first message wasn't a welcome: %s", data)
	}
	c.token.Store(w.Client.ReconnectToken)
	c.id = w.Client.ID
	go c.read()
	return c, nil
}

// Closed is closed once the connection to the server is lost.
func (c *Client) Closed() <-chan struct{} { return c.closed }

// Close ends the client and its connection.
func (c *Client) Close() error {
	c.end()
	err := c.ws.Close(websocket.StatusNormalClosure, "")
	c.mu.Lock()
	for id, ch := range c.pending {
		ch <- response{ID: id, Error: &apiError{Message: "connection to the server closed"}}
		delete(c.pending, id)
	}
	c.mu.Unlock()
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return err
}

// ClientID is the ID the server's welcome gave this connection.
func (c *Client) ClientID() string { return c.id }

// read takes messages off the connection: responses go to their calls,
// events to OnEvent.
func (c *Client) read() {
	for {
		typ, data, err := c.ws.Read(c.ctx)
		if err != nil {
			c.mu.Lock()
			c.err = err
			for id, ch := range c.pending {
				ch <- response{ID: id, Error: &apiError{Message: "connection to the server lost"}}
				delete(c.pending, id)
			}
			c.mu.Unlock()
			close(c.closed)
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var m struct {
			Type    string          `json:"type"`
			ID      string          `json:"id"`
			Seq     uint64          `json:"seq"`
			Payload json.RawMessage `json:"payload"`
			Error   *apiError       `json:"error"`
		}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "response":
			c.mu.Lock()
			ch := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- response{ID: m.ID, Seq: m.Seq, Payload: m.Payload, Error: m.Error}
			}
		case "event":
			c.seq.Store(m.Seq)
			if c.OnEvent != nil {
				c.OnEvent(c.decodeEvent(m.Seq, m.Payload))
			}
		}
	}
}

// decodeEvent turns an event envelope into an Event: a session_event's
// inner event, or one about the whole server.
func (c *Client) decodeEvent(seq uint64, raw json.RawMessage) Event {
	var outer struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	json.Unmarshal(raw, &outer)
	ev := Event{Type: outer.Type, Seq: seq, Payload: outer.Payload}
	if outer.Type == "session_event" {
		var se struct {
			SessionID string          `json:"session_id"`
			Event     json.RawMessage `json:"event"`
		}
		if json.Unmarshal(outer.Payload, &se) == nil {
			ev.SessionID = se.SessionID
			json.Unmarshal(se.Event, &outer)
			ev.Type, ev.Payload = outer.Type, outer.Payload
		}
	}
	switch ev.Type {
	case "block_event":
		var be struct {
			BlockID string          `json:"block_id"`
			Creator string          `json:"creator_name"`
			Name    string          `json:"name"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(ev.Payload, &be) == nil {
			ev.BlockID, ev.Creator, ev.Name, ev.Payload = be.BlockID, be.Creator, be.Name, be.Payload
		}
	case "block_created", "block_closed":
		var p struct {
			BlockID string `json:"block_id"`
		}
		json.Unmarshal(ev.Payload, &p)
		ev.BlockID = p.BlockID
	case "session_view_changed":
		var p struct {
			Revision uint64 `json:"revision"`
		}
		json.Unmarshal(ev.Payload, &p)
		ev.Revision = p.Revision
	}
	return ev
}

// Call sends a request for method with payload and decodes the result
// into out. The response's seq, the count of events ahead of it, goes to
// outSeq when set.
func (c *Client) Call(ctx context.Context, method string, payload, out any) (seq uint64, err error) {
	ch := make(chan response, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return 0, errors.New("rex: connection to the server lost")
	}
	c.nextID++
	id := fmt.Sprint(c.nextID)
	c.pending[id] = ch
	c.mu.Unlock()
	b, _ := json.Marshal(request{Type: "request", ID: id, Method: method, Payload: payload})
	werr := c.ws.Write(ctx, websocket.MessageText, b)
	if werr != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return 0, werr
	}
	var res response
	select {
	case res = <-ch:
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return 0, ctx.Err()
	}
	if res.Error != nil {
		return res.Seq, res.Error
	}
	if out != nil && len(res.Payload) > 0 {
		if err := json.Unmarshal(res.Payload, out); err != nil {
			return res.Seq, fmt.Errorf("rex: decoding %s's result: %w", method, err)
		}
	}
	return res.Seq, nil
}

// Bootstrap is what session.bootstrap returns.
type Bootstrap struct {
	ClientID     string       `json:"client_id"`
	ServerStatus ServerStatus `json:"server_status"`
	AttachAll    struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
			Label     string `json:"label"`
		} `json:"sessions"`
	} `json:"attach_all"`
	SelectedSession *struct {
		Session struct {
			SessionID string `json:"session_id"`
			Label     string `json:"label"`
		} `json:"session"`
		View SessionView `json:"view"`
	} `json:"selected_session"`
	DataTickets []DataTicket `json:"data_tickets"`
}

// ServerStatus is server.status's answer.
type ServerStatus struct {
	PID          int       `json:"pid"`
	InstanceID   string    `json:"instance_id"`
	HostID       string    `json:"host_id"`
	StartedAt    time.Time `json:"started_at"`
	ExitEmpty    bool      `json:"exit_empty"`
	SessionCount int       `json:"session_count"`
	APIRevision  int       `json:"api_revision"`
}

// DataTicket opens one data connection to a block.
type DataTicket struct {
	SessionID   string `json:"session_id"`
	BlockID     string `json:"block_id"`
	DataRefID   string `json:"data_ref_id"`
	Subprotocol string `json:"subprotocol"`
	Ticket      string `json:"ticket"`
	ExpiresInMs int    `json:"expires_in_ms"`
}

// Bootstrap starts the connection: the client receives every event,
// attaches to the session attach names (or to one the server picks when
// attach is empty), and gets data tickets for each terminal block it can
// see.
func (c *Client) Bootstrap(ctx context.Context, attach ...string) (*Bootstrap, error) {
	payload := map[string]any{"event_scope": "all"}
	if len(attach) > 0 {
		// An empty list attaches none; leaving the field out lets the
		// server pick a session.
		payload["attach_session_ids"] = attach
	}
	var b Bootstrap
	_, err := c.Call(ctx, "session.bootstrap", payload, &b)
	return &b, err
}

// SessionCreate makes a session with no windows.
func (c *Client) SessionCreate(ctx context.Context, label string) (string, error) {
	var res struct {
		SessionID string `json:"session_id"`
	}
	_, err := c.Call(ctx, "session.create", map[string]any{"label": label}, &res)
	return res.SessionID, err
}

// SessionCreateShell makes a session with one window running a shell in
// dir, as Rex makes its sessions.
func (c *Client) SessionCreateShell(ctx context.Context, label, dir string) (sid string, err error) {
	var res struct {
		SessionID string `json:"session_id"`
	}
	payload := map[string]any{
		"label": label,
		"initial_windows": []map[string]any{{
			"layout": blockSpec("shell", TerminalOptions{Dir: dir}.spec()),
		}},
	}
	_, err = c.Call(ctx, "session.create", payload, &res)
	return res.SessionID, err
}

// SessionDestroy ends a session and every block in it.
func (c *Client) SessionDestroy(ctx context.Context, sid string) error {
	_, err := c.Call(ctx, "session.destroy", map[string]any{"session_id": sid}, nil)
	return err
}

// SessionList names the server's sessions, in order.
func (c *Client) SessionList(ctx context.Context) ([]struct {
	SessionID string `json:"session_id"`
	Label     string `json:"label"`
}, error) {
	var res struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
			Label     string `json:"label"`
		} `json:"sessions"`
	}
	_, err := c.Call(ctx, "session.list", nil, &res)
	return res.Sessions, err
}

// Attach attaches the connection to a session and returns its view, and
// the seq of the last event ahead of it: events with a higher seq came
// after.
func (c *Client) Attach(ctx context.Context, sid string) (*SessionView, uint64, error) {
	var res struct {
		ClientID string      `json:"client_id"`
		View     SessionView `json:"view"`
	}
	seq, err := c.Call(ctx, "session.attach", map[string]any{"session_id": sid}, &res)
	return &res.View, seq, err
}

// View returns the session's current view, and the seq to compare events
// with.
func (c *Client) View(ctx context.Context, sid string) (*SessionView, uint64, error) {
	var v SessionView
	seq, err := c.Call(ctx, "session.view", map[string]any{"session_id": sid}, &v)
	return &v, seq, err
}

// blockSpec makes a layout node holding one terminal.
func blockSpec(label string, opts map[string]any) map[string]any {
	return map[string]any{"block": map[string]any{
		"flavor":  "com.superlogical.terminal.shell",
		"options": opts,
		"label":   label,
	}}
}

// TerminalOptions are the options a terminal flavor takes at creation.
type TerminalOptions struct {
	Command []string // runs instead of the login shell
	Dir     string   // the directory to start in
	Cols    int
	Rows    int
}

func (o TerminalOptions) spec() map[string]any {
	s := map[string]any{"cwd": o.Dir}
	if len(o.Command) > 0 {
		s["command"] = o.Command
		s["shell"] = "none"
	}
	if o.Cols > 0 && o.Rows > 0 {
		s["size"] = map[string]any{"columns": o.Cols, "rows": o.Rows}
	}
	return s
}

// NewWindow creates a window with one terminal.
func (c *Client) NewWindow(ctx context.Context, sid string, opts TerminalOptions) (wid, bid string, err error) {
	var res struct {
		WindowID string   `json:"window_id"`
		BlockIDs []string `json:"block_ids"`
	}
	_, err = c.Call(ctx, "session.new_window", map[string]any{
		"session_id": sid,
		"layout":     blockSpec("shell", opts.spec()),
		"focus":      true,
	}, &res)
	if len(res.BlockIDs) > 0 {
		bid = res.BlockIDs[0]
	}
	return res.WindowID, bid, err
}

// NewSplit creates a terminal beside anchor, which direction divides:
// "horizontal" puts the new block left or right of the anchor, side says
// which ("after" is right or below).
func (c *Client) NewSplit(ctx context.Context, sid, anchor, direction, side string, opts TerminalOptions, focus bool) (string, error) {
	var res struct {
		BlockIDs []string `json:"block_ids"`
	}
	_, err := c.Call(ctx, "session.new_split", map[string]any{
		"session_id": sid,
		"anchor_id":  anchor,
		"direction":  direction,
		"side":       side,
		"layout":     blockSpec("shell", opts.spec()),
		"focus":      focus,
	}, &res)
	if len(res.BlockIDs) == 0 {
		return "", err
	}
	return res.BlockIDs[0], err
}

// NewBlock creates a detached block, placed later with MoveBlock.
func (c *Client) NewBlock(ctx context.Context, sid string, opts TerminalOptions) (string, error) {
	var res struct {
		BlockID string `json:"block_id"`
	}
	_, err := c.Call(ctx, "session.new_block", map[string]any{
		"session_id": sid,
		"flavor":     "com.superlogical.terminal.shell",
		"options":    opts.spec(),
		"label":      "shell",
	}, &res)
	return res.BlockID, err
}

// MoveBlock places block beside anchor, as in NewSplit.
func (c *Client) MoveBlock(ctx context.Context, sid, block, anchor, direction, side string) error {
	_, err := c.Call(ctx, "session.move_block", map[string]any{
		"session_id":      sid,
		"block_id":        block,
		"anchor_block_id": anchor,
		"direction":       direction,
		"side":            side,
	}, nil)
	return err
}

// CloseBlock ends the program of a block, or of its window's focused one
// when block is empty.
func (c *Client) CloseBlock(ctx context.Context, sid, block string) error {
	payload := map[string]any{"session_id": sid}
	if block != "" {
		payload["block_id"] = block
	}
	_, err := c.Call(ctx, "block.close", payload, nil)
	return err
}

// CloseWindow closes a window and every block in it.
func (c *Client) CloseWindow(ctx context.Context, sid, wid string) error {
	_, err := c.Call(ctx, "session.close_window", map[string]any{"session_id": sid, "window_id": wid}, nil)
	return err
}

// FocusBlock focuses a block and makes its window the active one.
func (c *Client) FocusBlock(ctx context.Context, sid, block string) error {
	_, err := c.Call(ctx, "session.focus_block", map[string]any{"session_id": sid, "block_id": block}, nil)
	return err
}

// FocusWindow makes a window the active one.
func (c *Client) FocusWindow(ctx context.Context, sid, wid string) error {
	_, err := c.Call(ctx, "session.focus_window", map[string]any{"session_id": sid, "window_id": wid}, nil)
	return err
}

// MoveWindow moves a window before another, or last when before is "".
func (c *Client) MoveWindow(ctx context.Context, sid, wid, before string) error {
	_, err := c.Call(ctx, "session.move_window", map[string]any{
		"session_id": sid, "window_id": wid, "before_window_id": before,
	}, nil)
	return err
}

// ZoomBlock zooms a block to fill its window, or undoes the zoom.
func (c *Client) ZoomBlock(ctx context.Context, sid, block string) error {
	_, err := c.Call(ctx, "session.zoom_block", map[string]any{"session_id": sid, "block_id": block}, nil)
	return err
}

// ResizePane moves the border between two blocks, giving before the
// fraction ratio of their split's space.
func (c *Client) ResizePane(ctx context.Context, sid, before, after, direction string, ratio float64) error {
	_, err := c.Call(ctx, "session.resize_pane", map[string]any{
		"session_id": sid, "direction": direction,
		"before_block_id": before, "after_block_id": after, "ratio": ratio,
	}, nil)
	return err
}

// SetWindowLabel names a window; "" gives it the server's default.
func (c *Client) SetWindowLabel(ctx context.Context, sid, wid, label string) error {
	_, err := c.Call(ctx, "session.set_window_label", map[string]any{
		"session_id": sid, "window_id": wid, "label": label,
	}, nil)
	return err
}

// SetSessionLabel names the session.
func (c *Client) SetSessionLabel(ctx context.Context, sid, label string) error {
	_, err := c.Call(ctx, "session.set_label", map[string]any{"session_id": sid, "label": label}, nil)
	return err
}

// OwnBlock claims blocks for this client, which then sizes and colors
// them; mode is "take" or "request", which takes only unowned ones.
func (c *Client) OwnBlock(ctx context.Context, sid, mode string, blocks ...string) error {
	_, err := c.Call(ctx, "session.own_block", map[string]any{
		"session_id": sid, "block_ids": blocks, "mode": mode,
	}, nil)
	return err
}

// BlockMethod calls a method a block provides, such as
// com.superlogical.terminal.write, decoding its result into out. The
// server wants an object for args even when the method takes none.
func (c *Client) BlockMethod(ctx context.Context, method, sid, block string, args, out any) error {
	if args == nil {
		args = map[string]any{}
	}
	_, err := c.Call(ctx, method, map[string]any{
		"session_id": sid, "block_id": block, "args": args,
	}, out)
	return err
}

// Write types data into the block's terminal.
func (c *Client) Write(ctx context.Context, sid, block string, data []byte) error {
	return c.BlockMethod(ctx, "com.superlogical.terminal.write", sid, block,
		map[string]any{"data": string(data)}, nil)
}

// ResizeTerm asks a terminal's size to be cols by rows.
func (c *Client) ResizeTerm(ctx context.Context, sid, block string, cols, rows int) error {
	return c.BlockMethod(ctx, "com.superlogical.terminal.resize", sid, block,
		map[string]any{"columns": cols, "rows": rows}, nil)
}

// ProcessInfo is what terminal.process says of the block's program.
type ProcessInfo struct {
	LastExit *struct {
		ExitCode int    `json:"exit_code"`
		Error    string `json:"error"`
	} `json:"last_exit"`
	Child      *Proc  `json:"child"`
	Foreground *Proc  `json:"foreground"`
	Ancestors  []Proc `json:"ancestors"`
	Home       string `json:"home"`
}

// Proc describes one process, as terminal.process reports it.
type Proc struct {
	PID         int    `json:"pid"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	InvokedPath string `json:"invoked_path"`
	Argv0       string `json:"argv0"`
	Script      string `json:"script"`
	CWD         string `json:"cwd"`
}

// Process returns what runs in the block's terminal and how it ended.
func (c *Client) Process(ctx context.Context, sid, block string) (*ProcessInfo, error) {
	var p ProcessInfo
	err := c.BlockMethod(ctx, "com.superlogical.terminal.process", sid, block, nil, &p)
	return &p, err
}

// Title returns the title the block's terminal reports.
func (c *Client) Title(ctx context.Context, sid, block string) (string, error) {
	var res struct {
		Title string `json:"title"`
	}
	err := c.BlockMethod(ctx, "com.superlogical.terminal.title", sid, block, nil, &res)
	return res.Title, err
}

// Pwd returns the directory the block's terminal last reported.
func (c *Client) Pwd(ctx context.Context, sid, block string) (string, error) {
	var res struct {
		Pwd string `json:"pwd"`
	}
	err := c.BlockMethod(ctx, "com.superlogical.terminal.pwd", sid, block, nil, &res)
	return res.Pwd, err
}

// Size returns the terminal's size.
func (c *Client) Size(ctx context.Context, sid, block string) (cols, rows int, err error) {
	var res struct {
		Columns int `json:"columns"`
		Rows    int `json:"rows"`
	}
	err = c.BlockMethod(ctx, "com.superlogical.terminal.size", sid, block, nil, &res)
	return res.Columns, res.Rows, err
}

// DataConnect returns a ticket for a data connection to the block.
func (c *Client) DataConnect(ctx context.Context, sid, block string) (*DataTicket, error) {
	var t DataTicket
	_, err := c.Call(ctx, "data.connect", map[string]any{
		"session_id": sid, "block_id": block,
		"data_ref_id": "terminal", "subprotocol": "rex.data.terminal.v1",
	}, &t)
	return &t, err
}

// Info is what the app's panes show of a block's terminal: its program,
// where it works, its title, and whether it exited.
type Info struct {
	Program  string
	Argv0    string
	Shell    string
	Dir      string
	Title    string
	Idle     bool
	Exited   bool
	ExitCode int
	Cols     int
	Rows     int
	Home     string
}

// ProcessChanged is a terminal's process_changed event payload.
type ProcessChanged struct {
	Generation int    `json:"generation"`
	Foreground *Proc  `json:"foreground"`
	Ancestors  []Proc `json:"ancestors"`
}

// Fill updates in from a process_changed payload, or from what
// terminal.process returned when ev is nil.
func (in *Info) Fill(ev *ProcessChanged, pi *ProcessInfo) {
	var fg *Proc
	var an []Proc
	switch {
	case ev != nil:
		fg, an = ev.Foreground, ev.Ancestors
	case pi != nil:
		fg, an = pi.Foreground, pi.Ancestors
		if pi.Home != "" {
			in.Home = pi.Home
		}
		if pi.LastExit != nil {
			in.Exited, in.ExitCode = true, pi.LastExit.ExitCode
		}
	default:
		return
	}
	// The program that labels the pane: the foreground's readable name,
	// then its script, then argv0.
	if fg != nil {
		in.Program, in.Argv0 = fg.Name, fg.Argv0
		if fg.Script != "" {
			in.Program = fg.Script
		}
		if fg.CWD != "" {
			in.Dir = fg.CWD
		}
	}
	// The shell is the deepest process: the last ancestor, or the
	// foreground itself when nothing runs over it.
	in.Idle = len(an) == 0
	if in.Idle && fg != nil {
		in.Shell = fg.Name
		if in.Program == "" {
			in.Program = fg.Name
		}
	} else if len(an) > 0 {
		in.Shell = an[len(an)-1].Name
	}
}
