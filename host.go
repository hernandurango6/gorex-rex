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

// hostChip shows the server the window is on; a click opens the hosts,
// which switch it, add new ones and say more of the connection.
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
	if a.hostID != "" {
		if h := hosts.hostOf(a.hostID); h != nil {
			name = h.Label
		}
	}
	glyph := "server"
	if runtime.GOOS == "darwin" {
		glyph = "monitor"
	} else if runtime.GOOS == "windows" {
		glyph = "monitor"
	}
	if a.connecting != "" {
		model = "connecting…"
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
		ui.Icon(c, icon("chevron-down")).Size(13, 13).TextColor(k.textFaint)
	})
	ui.PopoverBase(c, chip, &a.hostOpen, func(panel ui.Element) {
		panel.Margin(8, 0, 0, 0).Width(320).Padding(10).Radius(14).Background(k.panel).Border(1, k.panelBorder).
			Shadow(0, 12, 32, 0, k.shadowFocused)
		a.hostPanel(c, k)
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

// hostPanel draws the hosts the window can move to: this machine first,
// then the saved ones, and the details of the server it is on.
func (a *App) hostPanel(c *ui.Context, k *colors) {
	ui.Text(c, "Servers").FontSize(12).FontWeight(700).TextColor(k.textFaint).Padding(2, 6, 6, 6)
	ui.Column(c).Gap(2).Children(func() {
		a.hostRow(c, k, nil)
		for _, h := range hosts.Hosts {
			a.hostRow(c, k, h)
		}
	})
	add := ui.Row(c).Gap(8).Padding(7, 10).Margin(4, 0, 0, 0).Radius(9).AlignItems(ui.Center).Cursor(ui.CursorPointer).
		Role(ui.RoleButton).Label("Add Server…")
	if add.Hovered() {
		add.Background(k.hover)
	}
	if add.Clicked() {
		a.hostOpen = false
		a.openHostDialog(nil)
	}
	add.Children(func() {
		ui.Icon(c, icon("plus")).Size(15, 15).TextColor(k.textMuted)
		ui.Text(c, "Add Server…").FontSize(13).FontWeight(500).TextColor(k.text)
	})
	ui.Divider(c).Margin(8, 0)
	a.serverDetails(c, k)
}

// hostRow is one server of the switcher: this machine's when h is nil.
// A click moves the window to it.
func (a *App) hostRow(c *ui.Context, k *colors, h *Host) {
	local := h == nil
	id := ""
	label, detail, glyph := "This computer", hostModel(), "laptop"
	if !local {
		id = h.ID
		label = h.Label
		detail = h.Endpoints[0]
		glyph = "server"
	}
	active := a.hostID == id
	connecting := a.connecting == hostIDOrLocal(id)
	row := ui.Row(c.Key(id)).Gap(10).Padding(7, 8).Radius(9).AlignItems(ui.Center).Cursor(ui.CursorPointer).
		Role(ui.RoleButton).Label(label)
	if row.Hovered() {
		row.Background(k.hover)
	}
	if active {
		row.Background(k.panelSel)
	}
	row.Children(func() {
		ui.Box(c).Size(30, 30).Radius(8).Background(k.hover).Center().Children(func() {
			ui.Icon(c, icon(glyph)).Size(17, 17).TextColor(k.textMuted)
		})
		ui.Column(c).Grow(1).MinWidth(0).Children(func() {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, label).FontSize(13).FontWeight(600).TextColor(k.text).SingleLine().Ellipsis("…")
				a.hostStatusDot(c, k, h, active, connecting)
			})
			ui.Text(c, detail).FontSize(11.5).TextColor(k.textFaint).SingleLine().Ellipsis("…")
		})
		if connecting {
			ui.Text(c, "connecting…").FontSize(11).TextColor(k.textFaint).Shrink(0)
		} else if active {
			ui.Icon(c, icon("check")).Size(14, 14).TextColor(k.busy).Shrink(0)
		} else if row.Hovered() && !local {
			// Edit and remove show only under the pointer, as the panes'
			// buttons do.
			if iconButton(c, k, "pencil", "Edit Server", 24, 13).Tooltip("Edit Server").Clicked() {
				a.hostOpen = false
				a.later(c, func() { a.openHostDialog(h) })
			}
			if iconButton(c, k, "trash-2", "Remove Server", 24, 14).Tooltip("Remove Server").Clicked() {
				a.later(c, func() { hosts.removeHost(h) })
			}
		}
	})
	if row.Clicked() && !active && !connecting {
		a.hostOpen = false
		a.later(c, func() { a.switchToServer(id) })
	}
}

// hostStatusDot is the host's state: green where the window is, amber
// while joining, red when the last try failed.
func (a *App) hostStatusDot(c *ui.Context, k *colors, h *Host, active, connecting bool) {
	switch {
	case connecting:
		ui.Box(c).Size(7, 7).Radius(4).Background(k.attention).Shrink(0).Tooltip("Connecting…")
	case active:
		ui.Box(c).Size(7, 7).Radius(4).Background(k.busy).Shrink(0).Tooltip("Connected")
	case h != nil && h.LastFail != "":
		ui.Box(c).Size(7, 7).Radius(4).Background(ui.Hex("#ef4444")).Shrink(0).Tooltip("Last connection failed: " + h.LastFail)
	}
}

// serverDetails says more of the server the window is on: its endpoint,
// uptime and API, and what the panes count.
func (a *App) serverDetails(c *ui.Context, k *colors) {
	row := func(label, value string) {
		if value == "" {
			return
		}
		ui.Row(c).Gap(12).Padding(1, 6).Children(func() {
			ui.Text(c, label).FontSize(12).TextColor(k.textFaint).Width(70)
			ui.Text(c, value).FontSize(12).TextColor(k.text).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
		})
	}
	ui.Column(c).Gap(5).Padding(0, 0, 2, 0).Children(func() {
		if ep, remote := a.endpoint(); remote {
			row("Endpoint", ep.String())
		} else if a.client != nil {
			row("Endpoint", "local")
		}
		if a.status.PID > 0 {
			row("Server", fmt.Sprintf("pid %d · up %s", a.status.PID, roundDur(time.Since(a.status.StartedAt))))
		}
		if a.status.APIRevision > 0 {
			row("API", fmt.Sprintf("revision %d", a.status.APIRevision))
		}
		if a.client != nil {
			row("Panes", fmt.Sprintf("%d in this session", countPanes(a)))
		}
	})
	ui.Text(c, "Panes live in the Rex server: quit GoRex and they keep running, and come back as they were when it opens again.").
		FontSize(11).TextColor(k.textFaint).LineHeight(1.35).Padding(6, 6, 2, 6)
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

// openHostDialog opens the Add Server dialog, or the Edit one when h is
// a saved host.
func (a *App) openHostDialog(h *Host) {
	a.editingHost = h
	a.addHostErr = ""
	if h != nil {
		a.addHostLabel, a.addHostEndpoint = h.Label, h.Endpoints[0]
	} else {
		a.addHostLabel, a.addHostEndpoint = "", ""
	}
	a.addHostOpen = true
	a.focusReq = nil // the dialog's field takes the focus
}

// hostDialog draws the Add Server dialog over the window: a name, the
// address as rex's -S takes it, and the mistake the input makes, live.
func (a *App) hostDialog(c *ui.Context, k *colors) {
	if !a.addHostOpen {
		return
	}
	title := "Add Server"
	if a.editingHost != nil {
		title = "Edit Server"
	}
	// The endpoint's mistake shows as it is typed, before any submit.
	liveErr := ""
	if ep := strings.TrimSpace(a.addHostEndpoint); ep == "" {
		liveErr = "An HTTP or HTTPS URL, a bare hostname or host:port, or a socket path."
	} else if err := rex.ValidateEndpoint(ep); err != nil {
		liveErr = err.Error()
	}
	ui.DialogBase(c, &a.addHostOpen, func(backdrop, panel ui.Element) {
		backdrop.Background(k.backdrop)
		panel.Width(420).Radius(16).Background(k.panel).Border(1, k.panelBorder).
			Shadow(0, 24, 60, -8, ui.RGBA(0, 0, 0, 0.28)).Padding(18).Gap(12)
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Box(c).Size(34, 34).Radius(9).Background(k.hover).Center().Children(func() {
				ui.Icon(c, icon("server")).Size(19, 19).TextColor(k.textMuted)
			})
			ui.Column(c).Gap(2).Children(func() {
				ui.Text(c, title).FontSize(15).FontWeight(700).TextColor(k.text)
				ui.Text(c, "The window moves to it once it answers.").FontSize(12).TextColor(k.textFaint)
			})
		})
		fieldLabel := func(s string) {
			ui.Text(c, s).FontSize(11.5).FontWeight(600).TextColor(k.textFaint)
		}
		fieldLabel("NAME")
		nameIn := ui.TextInputBase(c, &a.addHostLabel).FontSize(13.5).Placeholder("studio — empty names it from the address").
			Padding(7, 10).Radius(8).Background(k.hover)
		if a.editingHost == nil {
			nameIn = nameIn.AutoFocus()
		}
		fieldLabel("ADDRESS")
		epIn := ui.TextInputBase(c, &a.addHostEndpoint).FontSize(13.5).Placeholder("studio.tail1234.ts.net or host:7297").
			Padding(7, 10).Radius(8).Background(k.hover)
		if a.editingHost != nil {
			epIn = epIn.AutoFocus()
		}
		// Enter submits from either field, as one form.
		submit := nameIn.Submitted() || epIn.Submitted()
		if a.addHostErr != "" {
			ui.Text(c, a.addHostErr).FontSize(12).TextColor(ui.Hex("#ef4444")).LineHeight(1.3)
		} else if liveErr != "" {
			ui.Text(c, liveErr).FontSize(12).TextColor(k.textFaint).LineHeight(1.3)
		}
		ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
			cancel := ui.Row(c).Padding(6, 14).Radius(8).Cursor(ui.CursorPointer).Role(ui.RoleButton).Label("Cancel")
			if cancel.Hovered() {
				cancel.Background(k.hover)
			}
			cancel.Children(func() {
				ui.Text(c, "Cancel").FontSize(13).FontWeight(500).TextColor(k.textMuted)
			})
			if cancel.Clicked() {
				a.addHostOpen = false
			}
			ok := ui.Row(c).Padding(6, 14).Radius(8).Cursor(ui.CursorPointer).Role(ui.RoleButton).Label("Save")
			ep := strings.TrimSpace(a.addHostEndpoint)
			valid := ep != "" && rex.ValidateEndpoint(ep) == nil
			if valid {
				ok.Background(k.busy)
			} else {
				ok.Background(k.busy.Alpha(0.4))
			}
			ok.Children(func() {
				ui.Text(c, "Save").FontSize(13).FontWeight(600).TextColor(ui.Hex("#ffffff"))
			})
			if ok.Clicked() && valid {
				submit = true
			}
		})
		if submit {
			a.submitHostDialog()
		}
	})
}

// submitHostDialog saves the dialog's host. A new one is saved and the
// window moves to it; an edit keeps it in place, and the window moves to
// it again when it's the server the window is on, since its address may
// have changed.
func (a *App) submitHostDialog() {
	ep := strings.TrimSpace(a.addHostEndpoint)
	if err := rex.ValidateEndpoint(ep); err != nil {
		a.addHostErr = err.Error()
		return
	}
	if h := a.editingHost; h != nil {
		if err := hosts.editHost(h, a.addHostLabel, ep); err != nil {
			a.addHostErr = err.Error()
			return
		}
		a.addHostOpen = false
		if a.hostID == h.ID {
			a.switchToServer(h.ID)
		}
		return
	}
	h, err := hosts.addHost(a.addHostLabel, ep)
	if err != nil {
		a.addHostErr = err.Error()
		return
	}
	a.addHostOpen = false
	a.switchToServer(h.ID)
}
