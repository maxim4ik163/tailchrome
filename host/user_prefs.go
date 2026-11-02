package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/netip"
	"os"
	"path/filepath"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
)

// userPrefsFileName is the sidecar file, stored next to tsnet's own state,
// that remembers the preferences the user chose in the extension.
//
// tsnet.Server.Start always calls LocalBackend.Start with a freshly built
// ipn.NewPrefs() (only Hostname, WantRunning, ControlURL, RunWebClient and
// AdvertiseTags filled in), which replaces every persisted pref except
// Persist. Without this file a browser or helper restart therefore silently
// reset the coordination server, hostname, LAN access, Shields Up, advertised
// routes and "use subnet routes" — and left a node key issued by a custom
// coordination server paired with the default Tailscale control URL (#153).
const userPrefsFileName = "tailchrome-prefs.json"

const userPrefsVersion = 1

// savedUserPrefs is the on-disk shape of userPrefsFileName. A nil field means
// "not recorded", in which case the tsnet/Tailchrome default is kept.
type savedUserPrefs struct {
	Version                int       `json:"version"`
	ControlURL             *string   `json:"controlURL,omitempty"`
	Hostname               *string   `json:"hostname,omitempty"`
	RouteAll               *bool     `json:"routeAll,omitempty"`
	CorpDNS                *bool     `json:"corpDNS,omitempty"`
	ShieldsUp              *bool     `json:"shieldsUp,omitempty"`
	RunSSH                 *bool     `json:"runSSH,omitempty"`
	ExitNodeAllowLANAccess *bool     `json:"exitNodeAllowLANAccess,omitempty"`
	AdvertiseRoutes        *[]string `json:"advertiseRoutes,omitempty"`
}

// snapshotUserPrefs captures the user-controlled subset of prefs. The exit
// node selection is intentionally left out: the extension already restores
// it (lastExitNodeID) once the node is connected.
func snapshotUserPrefs(p ipn.PrefsView) savedUserPrefs {
	controlURL := controlURLForPrefs(p.ControlURL())
	hostname := p.Hostname()
	routeAll := p.RouteAll()
	corpDNS := p.CorpDNS()
	shieldsUp := p.ShieldsUp()
	runSSH := p.RunSSH()
	lanAccess := p.ExitNodeAllowLANAccess()

	advertised := p.AdvertiseRoutes()
	routes := make([]string, 0, advertised.Len())
	for i := 0; i < advertised.Len(); i++ {
		routes = append(routes, advertised.At(i).String())
	}

	return savedUserPrefs{
		Version:                userPrefsVersion,
		ControlURL:             &controlURL,
		Hostname:               &hostname,
		RouteAll:               &routeAll,
		CorpDNS:                &corpDNS,
		ShieldsUp:              &shieldsUp,
		RunSSH:                 &runSSH,
		ExitNodeAllowLANAccess: &lanAccess,
		AdvertiseRoutes:        &routes,
	}
}

// serverControlURL returns the control URL to hand to tsnet.Server before
// Start, or "" to use the default server. Passing it to tsnet (rather than
// only fixing prefs afterwards) keeps the control client from ever contacting
// the default server with a node key issued by a custom one.
func (s *savedUserPrefs) serverControlURL() string {
	if s == nil || s.ControlURL == nil {
		return ""
	}
	u := controlURLForPrefs(*s.ControlURL)
	if u == "" || !isValidControlURL(u) {
		return ""
	}
	return u
}

// serverHostname returns the hostname tsnet should start with.
func (s *savedUserPrefs) serverHostname(fallback string) string {
	if s == nil || s.Hostname == nil || *s.Hostname == "" {
		return fallback
	}
	return *s.Hostname
}

// startupPrefsWithSaved layers the saved user prefs over startupPrefs so the
// whole restore happens in the single EditPrefs call init already makes.
func startupPrefsWithSaved(s *savedUserPrefs) *ipn.MaskedPrefs {
	mp := startupPrefs()
	if s == nil {
		return mp
	}
	if s.Hostname != nil && *s.Hostname != "" {
		mp.HostnameSet = true
		mp.Prefs.Hostname = *s.Hostname
	}
	if s.RouteAll != nil {
		mp.RouteAllSet = true
		mp.Prefs.RouteAll = *s.RouteAll
	}
	if s.CorpDNS != nil {
		mp.CorpDNSSet = true
		mp.Prefs.CorpDNS = *s.CorpDNS
	}
	if s.ShieldsUp != nil {
		mp.ShieldsUpSet = true
		mp.Prefs.ShieldsUp = *s.ShieldsUp
	}
	if s.RunSSH != nil {
		mp.RunSSHSet = true
		mp.Prefs.RunSSH = *s.RunSSH
	}
	if s.ExitNodeAllowLANAccess != nil {
		mp.ExitNodeAllowLANAccessSet = true
		mp.Prefs.ExitNodeAllowLANAccess = *s.ExitNodeAllowLANAccess
	}
	if s.AdvertiseRoutes != nil {
		routes := make([]netip.Prefix, 0, len(*s.AdvertiseRoutes))
		for _, raw := range *s.AdvertiseRoutes {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil {
				log.Printf("ignoring invalid saved advertise route %q: %v", raw, err)
				continue
			}
			routes = append(routes, prefix)
		}
		mp.AdvertiseRoutesSet = true
		mp.Prefs.AdvertiseRoutes = routes
	}
	return mp
}

// loadUserPrefs reads the sidecar file. A missing file is not an error.
func loadUserPrefs(stateDir string) (*savedUserPrefs, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, userPrefsFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var saved savedUserPrefs
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, fmt.Errorf("parse %s: %w", userPrefsFileName, err)
	}
	if saved.Version != userPrefsVersion {
		return nil, fmt.Errorf("unsupported %s version %d", userPrefsFileName, saved.Version)
	}
	return &saved, nil
}

// writeUserPrefsFile atomically replaces the sidecar file.
func writeUserPrefsFile(stateDir string, data []byte) error {
	tmp, err := os.CreateTemp(stateDir, "."+userPrefsFileName+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(stateDir, userPrefsFileName))
}

// persistUserPrefs records the prefs of the current session. It is called
// from the IPN bus watcher, which only starts after init has restored the
// saved prefs, so tsnet's transient startup defaults are never recorded.
func (h *Host) persistUserPrefs(lc *local.Client, generation uint64, prefs ipn.PrefsView) {
	if !prefs.Valid() {
		return
	}
	h.sessionMu.RLock()
	current := h.lc == lc && h.sessionGeneration == generation
	stateDir := h.stateDir
	h.sessionMu.RUnlock()
	if !current || stateDir == "" {
		return
	}

	data, err := json.MarshalIndent(snapshotUserPrefs(prefs), "", "  ")
	if err != nil {
		log.Printf("failed to encode user prefs: %v", err)
		return
	}

	h.userPrefsMu.Lock()
	defer h.userPrefsMu.Unlock()
	if h.userPrefsDir == stateDir && bytes.Equal(h.userPrefsLast, data) {
		return
	}
	if err := writeUserPrefsFile(stateDir, data); err != nil {
		log.Printf("failed to save user prefs: %v", err)
		return
	}
	h.userPrefsDir = stateDir
	h.userPrefsLast = data
}
