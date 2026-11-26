// GoRex is a terminal of tabs and split panes, after Superlogical's Rex,
// in MyGo's native UI. Its shells run in a Rex session server — the same
// one Rex.app talks to — so that they outlive the app: quit it, open it
// again, and every tab and pane is back as it was, its programs still
// running.
//
//	rex server start    the session server, once
//	go tool mygo dev    the app, rebuilt as the code changes
package main

import (
	"context"
	"log"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"

	"gorex/internal/rex"
)

func main() {
	registerFonts()
	loadSettings()
	loadHosts()
	a := &App{}
	a.server = serverChoice()
	seedHosts()
	mygo.App.SetMenu(a.menu())
	mygo.App.WhenReady(a.open)
	mygo.App.OnActivate(func(hasVisibleWindows bool) {
		if !hasVisibleWindows && a.win.Load() == nil {
			a.open()
		}
	})
	mygo.App.OnWindowAllClosed(func() {
		if runtime.GOOS != "darwin" {
			mygo.App.Quit()
		}
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}

// serverChoice picks the server the app connects to: the --server
// option, then REX_SERVER, then the settings' last choice, and "" means
// this machine's socket.
func serverChoice() string {
	server := ""
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		if args[i] == "--server" && i+1 < len(args) {
			server = args[i+1]
			i++
		} else if s, ok := strings.CutPrefix(args[i], "--server="); ok {
			server = s
		}
	}
	if server != "" {
		return server
	}
	if s := rex.SpecEnv(); s != "" {
		return s
	}
	return prefs.Server
}

// open opens the window, which fills with the session once the server
// connection joins it. The window comes up first: a remote server may
// take seconds to answer, and the host switcher needs a place to show it.
func (a *App) open() {
	a.reset()
	opts := mygo.WindowOptions{
		Title:                "GoRex",
		Width:                1000,
		Height:               620,
		MinWidth:             560,
		MinHeight:            340,
		StateKey:             "main",
		TitleBarStyle:        mygo.TitleBarHidden,
		TrafficLightPosition: &mygo.Point{X: 16, Y: 15},
		BackgroundColor:      "light-dark(#efe1e6, #231e27)",
		Content:              ui.View(a.view),
	}
	if ownControls {
		// The title bar draws the window's controls (windowControls).
		opts.TitleBarStyle, opts.Frameless = mygo.TitleBarDefault, true
	}
	win := mygo.NewWindow(opts)
	a.win.Store(win)
	win.OnClosed(func() {
		a.quitting = true
		for _, t := range a.tabs {
			for _, p := range t.panes() {
				p.closed = true
				p.detach() // the session goes on
			}
		}
		if a.client != nil {
			a.client.Close()
		}
		a.win.Store(nil)
	})
	a.hostID = hostIDForSpec(a.server)
	go a.connectToServer([]string{a.server}, a.hostID)
	a.debugHook()
}

// hostIDForSpec is the saved host one of whose endpoints the spec is,
// "" when the spec is this machine's or nobody's.
func hostIDForSpec(spec string) string {
	if spec == "" {
		return ""
	}
	for _, h := range hosts.Hosts {
		for _, e := range h.Endpoints {
			if e == spec {
				return h.ID
			}
		}
	}
	return ""
}

// switchToServer moves the window to another server: the machine's own
// when hostID is "", or the saved host it names. The panes of the
// current session stay until the new one's view replaces them, since
// they keep running in their own server either way.
func (a *App) switchToServer(hostID string) {
	if hostID == a.hostID || a.connecting != "" && a.connecting == hostIDOrLocal(hostID) {
		return
	}
	var specs []string
	server := ""
	if hostID != "" {
		h := hosts.hostOf(hostID)
		if h == nil {
			return
		}
		specs = specForHost(h)
		server = h.Endpoints[0]
	} else {
		specs = []string{""}
	}
	a.update(func() {
		a.server = server
		prefs.Server = server
		saveSettings()
		a.connecting = hostIDOrLocal(hostID)
		a.err = ""
	})
	go a.connectToServer(specs, hostID)
}

// hostIDOrLocal spells "" as the connection state's marker for this
// machine's server, which "" can't distinguish from "nothing connecting".
func hostIDOrLocal(hostID string) string {
	if hostID == "" {
		return "local"
	}
	return hostID
}

// connectToServer joins the server the specs reach, off the main thread,
// and moves the window to it. hostID is the saved host the specs belong
// to, "" for this machine's server. A failure leaves the window where it
// was: its sessions keep running in their server.
func (a *App) connectToServer(specs []string, hostID string) {
	marker := hostIDOrLocal(hostID)
	a.update(func() { a.connecting = marker })
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	client, err := rex.ConnectAny(ctx, specs)
	if err == nil {
		// The streams decode their snapshots with the terminal's
		// libghostty-vt.
		client.GhosttyPath, _ = terminal.LibraryPath()
		var b *rex.Bootstrap
		var sid string
		var v *rex.SessionView
		b, sid, v, err = a.join(ctx, client)
		if err == nil {
			// Events flow once bootstrap attached: bind them before the
			// view applies, so a change between it and watch is not lost.
			a.bindEvents(client)
			a.update(func() {
				if a.connecting != marker {
					client.Close()
					return
				}
				old := a.client
				a.client = client
				if len(specs) == 1 && specs[0] == "" {
					a.server = ""
				} else if len(specs) > 0 {
					a.server = specs[0]
				}
				a.hostID = hostID
				a.status = b.ServerStatus
				a.sid = sid
				a.connecting = ""
				a.err = ""
				a.viewSeq = v.Revision
				a.viewMax = v.Revision
				a.viewDirty = false
				a.events = nil
				a.applyView(v)
				if len(a.tabs) == 0 {
					home, _ := os.UserHomeDir()
					a.newTab(home)
				}
				if h := hosts.hostOf(hostID); h != nil {
					h.LastEndpoint = client.Ep.String()
					h.LastFail = ""
					saveHosts()
				}
				if old != nil {
					old.Close()
				}
				if win := a.win.Load(); win != nil {
					go a.watch(win, client)
				}
			})
			cancel()
			return
		}
		client.Close()
	}
	cancel()
	a.update(func() {
		if a.connecting != marker {
			return
		}
		a.connecting = ""
		log.Printf("rex: connecting to %v: %v", specs, err)
		if h := hosts.hostOf(hostID); h != nil {
			h.LastFail = err.Error()
			saveHosts()
		}
		if a.client != nil {
			// The window keeps its server: the failed one simply didn't
			// answer.
			a.err = "Could not reach that server: " + err.Error()
			return
		}
		// Nothing was ever connected: the window is empty, and the hint
		// says how a server comes up.
		hint := "Start one with `rex server start`."
		if hostID != "" {
			hint = "Check the address, and that `rex server tailscale on` or a TCP listener serves the API there."
		}
		a.err = err.Error() + " — " + hint
	})
}

// reset forgets the state of a window that closed, keeping the server's
// connection, for the next window.
func (a *App) reset() {
	a.tabs, a.active, a.focusReq = nil, 0, nil
	a.paletteOpen, a.hostOpen, a.renaming = false, false, nil
	a.quitting, a.title, a.err = false, "", ""
	a.viewSeq, a.viewMax, a.viewDirty = 0, 0, false
	a.events = nil
	a.hostOpen, a.addHostOpen = false, false
}
