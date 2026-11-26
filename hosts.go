package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorex/internal/rex"
)

// Host is a saved remote server, as Rex.app keeps them: a name, the
// endpoints that reach it in the order to try, and what the last
// connection learned of it.
type Host struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Endpoints []string `json:"endpoints"`
	// LastEndpoint is the one the last successful connection used; the
	// next switch tries it first, as Rex.app's preferredEndpoint.
	LastEndpoint string `json:"lastEndpoint,omitempty"`
	// LastFail is the last connection error, "" after a success.
	LastFail string    `json:"lastFail,omitempty"`
	AddedAt  time.Time `json:"addedAt"`
}

// specs is the host's endpoints with the one that last worked first.
func (h *Host) specs() []string {
	if h.LastEndpoint == "" {
		return h.Endpoints
	}
	out := []string{h.LastEndpoint}
	for _, e := range h.Endpoints {
		if e != h.LastEndpoint {
			out = append(out, e)
		}
	}
	return out
}

// hostStore is the directory of saved hosts, kept in hosts.json of the
// app's settings.
type hostStore struct {
	Hosts []*Host `json:"hosts"`
}

var hosts hostStore

func hostsPath() string { return filepath.Join(rex.Dir(), "hosts.json") }

func loadHosts() {
	if b, err := os.ReadFile(hostsPath()); err == nil {
		json.Unmarshal(b, &hosts)
	}
}

func saveHosts() {
	b, _ := json.MarshalIndent(hosts, "", "  ")
	os.MkdirAll(rex.Dir(), 0o700)
	os.WriteFile(hostsPath(), b, 0o600)
}

func newHostID() string {
	return fmt.Sprintf("host:%x", time.Now().UnixNano())
}

// hostOf returns the host id names, nil when it isn't saved.
func (s *hostStore) hostOf(id string) *Host {
	for _, h := range s.Hosts {
		if h.ID == id {
			return h
		}
	}
	return nil
}

// addHost validates and saves a host with one endpoint, returning the
// mistake the input has. A host already saved at the same endpoint is an
// error too, as Rex.app merges them: one entry per server.
func (s *hostStore) addHost(label, endpoint string) (*Host, error) {
	label = strings.TrimSpace(label)
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("the server needs an address")
	}
	if err := rex.ValidateEndpoint(endpoint); err != nil {
		return nil, err
	}
	for _, h := range s.Hosts {
		for _, e := range h.Endpoints {
			if e == endpoint {
				return nil, fmt.Errorf("this server is already saved as %q; connect to it from the list", h.Label)
			}
		}
	}
	if label == "" {
		label = endpointLabel(endpoint)
	}
	h := &Host{ID: newHostID(), Label: label, Endpoints: []string{endpoint}, AddedAt: time.Now()}
	s.Hosts = append(s.Hosts, h)
	saveHosts()
	return h, nil
}

// editHost renames a host and moves it to a new endpoint, when they
// change.
func (s *hostStore) editHost(h *Host, label, endpoint string) error {
	label = strings.TrimSpace(label)
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return fmt.Errorf("the server needs an address")
	}
	if err := rex.ValidateEndpoint(endpoint); err != nil {
		return err
	}
	for _, o := range s.Hosts {
		if o == h {
			continue
		}
		for _, e := range o.Endpoints {
			if e == endpoint {
				return fmt.Errorf("this server is already saved as %q", o.Label)
			}
		}
	}
	if label == "" {
		label = endpointLabel(endpoint)
	}
	h.Label = label
	h.Endpoints = []string{endpoint}
	saveHosts()
	return nil
}

func (s *hostStore) removeHost(h *Host) {
	for i, o := range s.Hosts {
		if o == h {
			s.Hosts = append(s.Hosts[:i], s.Hosts[i+1:]...)
			saveHosts()
			return
		}
	}
}

// endpointLabel derives a host's name from its endpoint: the first label
// of the host part, which for a tailnet name is the machine's own.
func endpointLabel(endpoint string) string {
	s := endpoint
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/@"); i >= 0 {
		s = s[:i]
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	s = strings.TrimSuffix(strings.Trim(s, "[]"), ".")
	if i := strings.Index(s, "."); i > 0 {
		s = s[:i]
	}
	if s == "" {
		return "server"
	}
	return s
}

// seedHosts puts the server of the environment or the settings into the
// directory once, so the first switcher the user opens shows where they
// already connected.
func seedHosts() {
	if len(hosts.Hosts) > 0 {
		return
	}
	server := rex.SpecEnv()
	if server == "" {
		server = prefs.Server
	}
	if server == "" {
		return
	}
	if _, err := hosts.addHost("", server); err != nil {
		hosts.Hosts = nil // a bad spec seeds nothing
		saveHosts()
	}
}

// hostStatus is how a host shows in the switcher: the one the window is
// on, one being connected to, or one that failed last time.
type hostStatus int

const (
	hostIdle hostStatus = iota
	hostActive
	hostConnecting
	hostFailed
)

// currentHostID is the saved host the window is connected to or joining,
// "" for this machine's server.
func (a *App) currentHostID() string {
	return a.hostID
}

func (a *App) hostStatus(h *Host) hostStatus {
	switch {
	case a.connecting == h.ID:
		return hostConnecting
	case a.hostID == h.ID:
		return hostActive
	case h.LastFail != "":
		return hostFailed
	}
	return hostIdle
}

// specForHost resolves what ConnectAny tries for a host: its endpoints,
// last-working first.
func specForHost(h *Host) []string {
	return h.specs()
}

// probeHost checks the host answers, without disturbing anything: a
// connection that closes right after its bootstrap.
func probeHost(ctx context.Context, h *Host) (endpoint string, err error) {
	c, err := rex.ConnectAny(ctx, specForHost(h))
	if err != nil {
		return "", err
	}
	ep := c.Ep.String()
	c.Close()
	return ep, nil
}
