// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package config

import (
	"strings"
	"testing"
)

// TestLoadDepotParsesBothCoordinates verifies the yard location is real
// configuration: DEPOT_LAT/DEPOT_LNG land on the Config the workflow reads.
func TestLoadDepotParsesBothCoordinates(t *testing.T) {
	t.Setenv("AUTH_MODE", "dev")
	t.Setenv("DEPOT_LAT", "32.7767")
	t.Setenv("DEPOT_LNG", "-96.7970")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DepotLat == nil || cfg.DepotLng == nil {
		t.Fatal("DEPOT_LAT/DEPOT_LNG must populate the config")
	}
	if *cfg.DepotLat != 32.7767 || *cfg.DepotLng != -96.7970 {
		t.Fatalf("depot = (%v, %v), want (32.7767, -96.7970)", *cfg.DepotLat, *cfg.DepotLng)
	}
}

// TestLoadDepotUnsetIsNil verifies an unconfigured install boots — the depot is
// simply absent, and the workflow falls back to the stop centroid rather than
// to any built-in coordinate.
func TestLoadDepotUnsetIsNil(t *testing.T) {
	t.Setenv("AUTH_MODE", "dev")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("an install with no depot configured must still boot: %v", err)
	}
	if cfg.DepotLat != nil || cfg.DepotLng != nil {
		t.Fatalf("expected no depot, got (%v, %v)", cfg.DepotLat, cfg.DepotLng)
	}
}

// TestLoadDepotFailsClosed verifies a half-supplied, unparseable, or
// out-of-range depot is a hard boot error — never a silently-wrong routing
// origin. This mirrors the DATABASE_URL / SESSION_SECRET fail-closed style.
func TestLoadDepotFailsClosed(t *testing.T) {
	cases := []struct {
		name     string
		lat, lng string
		setLat   bool
		setLng   bool
		wantIn   string
	}{
		{name: "latitude without longitude", lat: "32.7767", setLat: true, wantIn: "together"},
		{name: "longitude without latitude", lng: "-96.7970", setLng: true, wantIn: "together"},
		{name: "unparseable latitude", lat: "north", lng: "-96.7970", setLat: true, setLng: true, wantIn: "DEPOT_LAT"},
		{name: "unparseable longitude", lat: "32.7767", lng: "", setLat: true, setLng: true, wantIn: "DEPOT_LNG"},
		{name: "latitude out of range", lat: "132.7", lng: "-96.79", setLat: true, setLng: true, wantIn: "out of range"},
		{name: "longitude out of range", lat: "32.7", lng: "-196.79", setLat: true, setLng: true, wantIn: "out of range"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AUTH_MODE", "dev")
			if tc.setLat {
				t.Setenv("DEPOT_LAT", tc.lat)
			}
			if tc.setLng {
				t.Setenv("DEPOT_LNG", tc.lng)
			}

			cfg, err := Load()
			if err == nil {
				t.Fatalf("a bad depot must refuse to boot, got depot (%v, %v)", cfg.DepotLat, cfg.DepotLng)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("error %q should explain the problem (%q)", err.Error(), tc.wantIn)
			}
		})
	}
}
