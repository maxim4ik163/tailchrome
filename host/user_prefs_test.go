package main

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"tailscale.com/ipn"
)

func TestUserPrefsRoundTripRestoresChoices(t *testing.T) {
	prefs := ipn.NewPrefs()
	prefs.ControlURL = "https://headscale.example.com"
	prefs.Hostname = "my-browser"
	prefs.RouteAll = false
	prefs.ShieldsUp = true
	prefs.ExitNodeAllowLANAccess = true
	prefs.AdvertiseRoutes = []netip.Prefix{netip.MustParsePrefix("10.20.0.0/16")}

	dir := t.TempDir()
	data, err := json.Marshal(snapshotUserPrefs(prefs.View()))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeUserPrefsFile(dir, data); err != nil {
		t.Fatal(err)
	}

	saved, err := loadUserPrefs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if saved == nil {
		t.Fatal("expected saved prefs")
	}
	if got := saved.serverControlURL(); got != "https://headscale.example.com" {
		t.Fatalf("serverControlURL = %q", got)
	}
	if got := saved.serverHostname("browser-ext"); got != "my-browser" {
		t.Fatalf("serverHostname = %q", got)
	}

	mp := startupPrefsWithSaved(saved)
	if !mp.RouteAllSet || mp.Prefs.RouteAll {
		t.Fatal("saved RouteAll=false must override the startup default")
	}
	if !mp.ShieldsUpSet || !mp.Prefs.ShieldsUp {
		t.Fatal("ShieldsUp not restored")
	}
	if !mp.ExitNodeAllowLANAccessSet || !mp.Prefs.ExitNodeAllowLANAccess {
		t.Fatal("ExitNodeAllowLANAccess not restored")
	}
	if !mp.HostnameSet || mp.Prefs.Hostname != "my-browser" {
		t.Fatal("Hostname not restored")
	}
	if !mp.AdvertiseRoutesSet || len(mp.Prefs.AdvertiseRoutes) != 1 ||
		mp.Prefs.AdvertiseRoutes[0] != netip.MustParsePrefix("10.20.0.0/16") {
		t.Fatalf("AdvertiseRoutes not restored: %v", mp.Prefs.AdvertiseRoutes)
	}
}

func TestUserPrefsDefaultControlURLStartsWithoutOverride(t *testing.T) {
	prefs := ipn.NewPrefs()
	prefs.ControlURL = "https://controlplane.tailscale.com"
	snap := snapshotUserPrefs(prefs.View())
	if snap.ControlURL == nil || *snap.ControlURL != "" {
		t.Fatalf("default control URL should be stored as empty, got %v", snap.ControlURL)
	}
	if got := snap.serverControlURL(); got != "" {
		t.Fatalf("serverControlURL = %q, want empty", got)
	}
}

func TestUserPrefsMissingOrInvalid(t *testing.T) {
	dir := t.TempDir()
	saved, err := loadUserPrefs(dir)
	if err != nil || saved != nil {
		t.Fatalf("missing file: saved=%v err=%v", saved, err)
	}
	if got := saved.serverHostname("browser-ext"); got != "browser-ext" {
		t.Fatalf("nil saved hostname = %q", got)
	}
	if got := saved.serverControlURL(); got != "" {
		t.Fatalf("nil saved control URL = %q", got)
	}
	mp := startupPrefsWithSaved(saved)
	if !mp.RouteAllSet || !mp.Prefs.RouteAll {
		t.Fatal("without saved prefs the startup defaults must apply")
	}

	if err := os.WriteFile(filepath.Join(dir, userPrefsFileName), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadUserPrefs(dir); err == nil {
		t.Fatal("expected an error for a corrupt file")
	}

	bad := "ftp://example.com"
	s := &savedUserPrefs{Version: userPrefsVersion, ControlURL: &bad}
	if got := s.serverControlURL(); got != "" {
		t.Fatalf("invalid control URL must be ignored, got %q", got)
	}
}
