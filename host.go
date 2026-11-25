package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"gorex/internal/rex"
)

// hostChip shows the machine the sessions run on; a click shows more of
// it, and of the session server.
func (a *App) hostChip(c *ui.Context, k *colors) {
	name, _ := os.Hostname()
	if i := strings.Index(name, "."); i > 0 {
		name = name[:i]
	}
	if name == "" {
		name = "Rex"
	}
	model := hostModel()
	if ep, remote := a.endpoint(); remote {
		// A remote server: the endpoint's host names it better than this
		// machine does, and its system is unknown.
		name = ep.Name()
		model = "remote server"
	}
	glyph := "server"
	if runtime.GOOS == "darwin" {
		glyph = "monitor"
	} else if runtime.GOOS == "windows" {
		glyph = "monitor"
	}
	chip := ui.Row(c).Gap(9).Padding(3, 8, 3, 6).Radius(9).AlignItems(ui.Center).Cursor(ui.CursorPointer).
		Role(ui.RoleButton).Label("Host " + name).MaxWidth(190).Shrink(0)
	if chip.Hovered() || a.hostOpen {
		chip.Background(k.hover)
	}
	if chip.Clicked() {
		a.hostOpen = !a.hostOpen
	}
	chip.Children(func() {
		ui.Icon(c, icon(glyph)).Size(19, 19).TextColor(k.textMuted)
		ui.Column(c).MinWidth(0).Children(func() {
			ui.Text(c, name).FontSize(13.5).FontWeight(700).TextColor(k.text.Alpha(0.85)).SingleLine().Ellipsis("…")
			ui.Text(c, model).FontSize(11.5).TextColor(k.textFaint).SingleLine().Ellipsis("…")
		})
	})
	ui.PopoverBase(c, chip, &a.hostOpen, func(panel ui.Element) {
		panel.Margin(8, 0, 0, 0).Width(300).Padding(14).Radius(14).Background(k.panel).Border(1, k.panelBorder).
			Shadow(0, 12, 32, 0, k.shadowFocused).Gap(12)
		a.hostPanel(c, k, name, glyph)
	})
}

// hostModel describes the machine the server runs on.
func hostModel() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS · " + runtime.GOARCH
	case "windows":
		return "Windows · " + runtime.GOARCH
	}
	return "Linux · " + runtime.GOARCH
}

func (a *App) hostPanel(c *ui.Context, k *colors, name, glyph string) {
	ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Size(38, 38).Radius(10).Background(k.hover).Center().Children(func() {
			ui.Icon(c, icon(glyph)).Size(24, 24).TextColor(k.text)
		})
		ui.Column(c).Grow(1).MinWidth(0).Children(func() {
			ui.Text(c, name).FontSize(14).FontWeight(700).TextColor(k.text).SingleLine().Ellipsis("…")
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Box(c).Size(7, 7).Radius(4).Background(k.busy)
				ui.Text(c, a.hostWhere()+" · connected").FontSize(12).TextColor(k.textFaint)
			})
		})
	})
	ui.Divider(c)
	row := func(label, value string) {
		if value == "" {
			return
		}
		ui.Row(c).Gap(12).Children(func() {
			ui.Text(c, label).FontSize(12.5).TextColor(k.textFaint).Width(78)
			ui.Text(c, value).FontSize(12.5).TextColor(k.text).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
		})
	}
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	ui.Column(c).Gap(7).Children(func() {
		if ep, remote := a.endpoint(); remote {
			row("Endpoint", ep.String())
			row("Client", thisMachine()+" · "+user)
		} else {
			row("System", hostModel())
			row("User", user)
		}
	})
	ui.Divider(c)
	running := 0
	for _, t := range a.tabs {
		for _, p := range t.panes() {
			if !p.info.Idle && p.info.Program != "" {
				running++
			}
		}
	}
	ui.Column(c).Gap(7).Children(func() {
		if a.status.PID > 0 {
			row("Server", fmt.Sprintf("pid %d · up %s", a.status.PID, roundDur(time.Since(a.status.StartedAt))))
		}
		if a.status.APIRevision > 0 {
			row("API", fmt.Sprintf("revision %d", a.status.APIRevision))
		}
		row("Panes", fmt.Sprintf("%d in this session, %d running a program", countPanes(a), running))
	})
	ui.Text(c, "Panes live in the Rex server: quit GoRex and they keep running, and come back as they were when it opens again.").
		FontSize(11.5).TextColor(k.textFaint).LineHeight(1.35)
}

func countPanes(a *App) int {
	n := 0
	for _, t := range a.tabs {
		n += len(t.panes())
	}
	return n
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// thisMachine names the machine the app runs on, as its system does.
func thisMachine() string {
	switch runtime.GOOS {
	case "darwin":
		return "This Mac"
	case "windows":
		return "This PC"
	}
	return "This computer"
}

// endpoint is the server's endpoint once the app is connected; remote is
// false for the machine's socket and before a connection exists.
func (a *App) endpoint() (rex.Endpoint, bool) {
	if a.client == nil {
		return rex.Endpoint{}, false
	}
	return a.client.Ep, a.client.Ep.Remote()
}

// hostWhere says where the sessions run: here, or on a remote server.
func (a *App) hostWhere() string {
	if _, remote := a.endpoint(); remote {
		return "remote server"
	}
	return thisMachine()
}
