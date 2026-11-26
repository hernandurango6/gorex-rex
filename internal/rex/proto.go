// Package rex is GoRex's client of a Rex session server.
//
// The server owns the terminals: shells keep running when the app quits,
// and the next launch attaches to them again. The app talks to it over a
// WebSocket: a control connection of JSON messages (requests, responses,
// and events), and one data connection per block that carries the
// terminal's output one way, with input sent as method calls on the
// control connection.
package rex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// APIRevision is the newest revision of the Rex API this client was
// written for; the server picks the newest both sides speak.
const APIRevision = 19

// Envelopes of the control connection.
type hello struct {
	Type           string      `json:"type"`
	APIRevision    int         `json:"api_revision"`
	APIMinRevision int         `json:"api_min_revision"`
	ReconnectToken string      `json:"reconnect_token,omitempty"`
	Client         clientHello `json:"client"`
}

type clientHello struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Label   string `json:"label,omitempty"`
}

type welcome struct {
	Type         string `json:"type"`
	Protocol     string `json:"protocol"`
	APIRevision  int    `json:"api_revision"`
	Resumed      bool   `json:"resumed"`
	Reconnecting bool   `json:"reconnecting"`
	MaxMsg       int    `json:"-"`
	Limits       struct {
		MaxMessageBytes int `json:"max_message_bytes"`
	} `json:"limits"`
	Client struct {
		ID             string `json:"id"`
		ReconnectToken string `json:"reconnect_token"`
	} `json:"client"`
}

type request struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Payload any    `json:"payload,omitempty"`
}

type response struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Seq     uint64          `json:"seq"`
	Payload json.RawMessage `json:"payload"`
	Error   *apiError       `json:"error"`
}

// apiError is the error of a failed call; Code is what to branch on.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

func (e *apiError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Event is a message the server sends without being asked: a session
// event carries the event of its session in Type, and a block event also
// the block and the name its creator gave it.
type Event struct {
	Type      string `json:"-"`
	Seq       uint64 `json:"-"`
	SessionID string `json:"-"`
	// BlockID, Creator and Name describe a block event: the block that
	// sent it, who made it, and the event's name in the creator's list,
	// such as title_changed.
	BlockID  string          `json:"-"`
	Creator  string          `json:"-"`
	Name     string          `json:"-"`
	Payload  json.RawMessage `json:"-"`
	Revision uint64          `json:"-"` // session_view_changed only
}

// SessionView is what session.view returns: the session's windows, the
// splits of their layouts, and the blocks placed in them.
type SessionView struct {
	Revision       uint64   `json:"revision"`
	SessionID      string   `json:"session_id"`
	Label          string   `json:"label"`
	ActiveWindowID string   `json:"active_window_id"`
	Windows        []Window `json:"windows"`
}

// Window is one window of the view: a tab, in order with the others.
type Window struct {
	WindowID       string  `json:"window_id"`
	Label          string  `json:"label"`
	FocusedBlockID string  `json:"focused_block_id"`
	ZoomedBlockID  string  `json:"zoomed_block_id"`
	Layers         []Layer `json:"layers"`
}

// Layer is the tiled layer that fills the window, or a floating one over
// it, which this client doesn't show.
type Layer struct {
	LayerID string     `json:"layer_id"`
	Kind    string     `json:"kind"`
	Bounds  [4]float64 `json:"bounds"`
	State   string     `json:"state"`
	Layout  *ViewNode  `json:"layout"`
	Blocks  []Block    `json:"blocks"`
}

// ViewNode is a node of a layer's layout: a block, or a split of two.
type ViewNode struct {
	Rect    [4]float64 `json:"rect"`
	BlockID string     `json:"block_id"`
	Split   *ViewSplit `json:"split"`
}

// ViewSplit divides a node's space in two.
type ViewSplit struct {
	SplitID   string    `json:"split_id"`
	Direction string    `json:"direction"` // "horizontal" puts Before left of After
	Ratio     float64   `json:"ratio"`
	Before    *ViewNode `json:"before"`
	After     *ViewNode `json:"after"`
}

// Block describes a block of the session, such as one terminal.
type Block struct {
	BlockID     string `json:"block_id"`
	Label       string `json:"label"`
	CreatorName string `json:"creator_name"`
	Flavor      string `json:"flavor"`
	HostID      string `json:"host_id"`
	Owner       string `json:"owner"`
	Placement   string `json:"placement"` // session.list_blocks only: "placed" or "detached"
	WindowID    string `json:"window_id"`
}

// Dir returns the directory of the app's settings.
func Dir() string {
	if d := os.Getenv("GOREX_DIR"); d != "" {
		return d
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "GoRex")
}

// SocketPath returns the Unix socket a Unix server listens on: the one
// REX_SOCKET names, or Rex's default for the user. Windows's server
// listens on a named pipe instead; defaultEndpoint picks it.
func SocketPath() string {
	if p := os.Getenv("REX_SOCKET"); p != "" {
		return p
	}
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return filepath.Join(base, "rex", "server.sock")
}
