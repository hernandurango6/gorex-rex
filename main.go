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
	a := &App{}
	a.server = serverChoice()
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

// open opens the window on the session the server keeps, or a new one.
func (a *App) open() {
	a.reset()
	client, err := rex.Connect(context.Background(), a.server)
	if err != nil {
		log.Print(err)
		hint := "Start one with `rex server start`."
		if a.server != "" {
			hint = "Check the address, and that `rex server tailscale on` or a TCP listener serves the API there."
		}
		mygo.Dialog.Error("GoRex could not reach the Rex server",
			err.Error()+"\n\n"+hint)
		mygo.App.Quit()
		return
	}
	a.rememberServer()
	// The streams decode their snapshots with the terminal's
	// libghostty-vt.
	client.GhosttyPath, _ = terminal.LibraryPath()
	a.client = client
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	b, sid, v, err := a.join(ctx, client)
	cancel()
	if err != nil {
		log.Print(err)
		client.Close()
		mygo.Dialog.Error("GoRex could not join a Rex session", err.Error())
		mygo.App.Quit()
		return
	}
	a.status = b.ServerStatus
	a.sid = sid
	// Events flow once bootstrap attached: bind them before the view
	// applies, so a change between it and watch is not lost.
	a.bindEvents(client)
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
	a.viewSeq = v.Revision
	a.applyView(v)
	if len(a.tabs) == 0 {
		home, _ := os.UserHomeDir()
		a.newTab(home)
	}
	go a.watch(win, client)
	a.debugHook()
	win.OnClosed(func() {
		a.quitting = true
		for _, t := range a.tabs {
			for _, p := range t.panes() {
				p.closed = true
				p.detach() // the session goes on
			}
		}
		a.client.Close()
		a.win.Store(nil)
	})
}

// reset forgets the state of a window that closed, keeping the server's
// connection, for the next window.
func (a *App) reset() {
	a.tabs, a.active, a.focusReq = nil, 0, nil
	a.paletteOpen, a.hostOpen, a.renaming = false, false, nil
	a.quitting, a.title = false, ""
	a.viewSeq, a.viewMax, a.viewDirty = 0, 0, false
	a.events = nil
}
