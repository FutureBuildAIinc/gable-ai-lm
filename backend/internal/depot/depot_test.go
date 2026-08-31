// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package depot

import (
	"math"
	"strings"
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

func fptr(f float64) *float64 { return &f }

// The depot this codebase once hardcoded — one specific dealer's yard in
// Kelowna BC. No rung of this ladder may ever produce it for a dealer that did
// not ask for it: every route, distance, duration and shift check is rooted
// here.
const (
	retiredHardcodedLat = 49.8863
	retiredHardcodedLng = -119.4666
)

// The dealer's yards as GableLBM reports them. Plano has never been geocoded:
// GableLBM omits the coordinate keys entirely, so they arrive as nil — which is
// a reason to fall back, never a place to root a route.
const (
	dallasYardID    = "77777777-7777-4777-8777-000000000001"
	fortWorthYardID = "77777777-7777-4777-8777-000000000002"
	planoYardID     = "77777777-7777-4777-8777-000000000003"
	closedYardID    = "77777777-7777-4777-8777-000000000009"

	dallasYardLat = 32.7767
	dallasYardLng = -96.7970
)

func texasBranches() []gable.Location {
	return []gable.Location{
		{ID: dallasYardID, Name: "Dallas Yard", Address: "1 Commerce St, Dallas, TX",
			Latitude: fptr(dallasYardLat), Longitude: fptr(dallasYardLng)},
		{ID: fortWorthYardID, Name: "Fort Worth Yard", Address: "2 Main St, Fort Worth, TX",
			Latitude: fptr(32.7555), Longitude: fptr(-97.3308)},
		// Never geocoded — nil, NOT 0,0.
		{ID: planoYardID, Name: "Plano Yard", Address: "3 Legacy Dr, Plano, TX"},
	}
}

// texasStops are two routable stops whose centroid is (32.80, -97.00).
func texasStops() []Point {
	return []Point{{Lat: 32.70, Lng: -96.90}, {Lat: 32.90, Lng: -97.10}}
}

// TestResolveEveryRung walks the whole ladder — REQUEST, BRANCH, CONFIG,
// CENTROID, NONE — and every way the branch rung can be wanted and unusable.
// The note is asserted alongside the coordinate because a fallback nobody is
// told about is how a route silently starts at the wrong yard.
func TestResolveEveryRung(t *testing.T) {
	cfgLat, cfgLng := fptr(30.25), fptr(-97.75)

	cases := []struct {
		name             string
		in               Input
		wantLat, wantLng float64
		wantSource       string
		// wantNote is a substring the reason must contain; empty means the note
		// must be empty, because nothing was declined.
		wantNote string
	}{
		{
			name: "request wins over branch, config and stops",
			in: Input{
				RequestLat: fptr(29.76), RequestLng: fptr(-95.36),
				BranchIDs: []string{dallasYardID}, Branches: texasBranches(),
				ConfigLat: cfgLat, ConfigLng: cfgLng, Stops: texasStops(),
			},
			wantLat: 29.76, wantLng: -95.36, wantSource: SourceRequest,
		},
		{
			name: "the branch this run ships from wins over the configured yard",
			in: Input{
				BranchIDs: []string{dallasYardID}, Branches: texasBranches(),
				ConfigLat: cfgLat, ConfigLng: cfgLng, Stops: texasStops(),
			},
			wantLat: dallasYardLat, wantLng: dallasYardLng, wantSource: SourceBranch,
		},
		{
			name:    "the same branch named by every order is still one yard",
			in:      Input{BranchIDs: []string{dallasYardID, dallasYardID, dallasYardID}, Branches: texasBranches()},
			wantLat: dallasYardLat, wantLng: dallasYardLng, wantSource: SourceBranch,
		},
		{
			name:    "configured yard wins over the centroid",
			in:      Input{ConfigLat: cfgLat, ConfigLng: cfgLng, Stops: texasStops()},
			wantLat: 30.25, wantLng: -97.75, wantSource: SourceConfig,
		},
		{
			name:    "centroid of the routable stops when nothing is configured",
			in:      Input{Stops: texasStops()},
			wantLat: 32.80, wantLng: -97.00, wantSource: SourceCentroid,
		},
		{
			name:    "a half-supplied request depot is ignored, not half-applied",
			in:      Input{RequestLat: fptr(29.76), Stops: texasStops()},
			wantLat: 32.80, wantLng: -97.00, wantSource: SourceCentroid,
		},
		{
			name:    "a half-configured depot is ignored, not half-applied",
			in:      Input{ConfigLat: cfgLat, Stops: texasStops()},
			wantLat: 32.80, wantLng: -97.00, wantSource: SourceCentroid,
		},
		{
			name:       "nothing to root on",
			in:         Input{},
			wantLat:    0,
			wantLng:    0,
			wantSource: SourceNone,
		},
		{
			name:    "no branch named at all is silent, exactly as before branches existed",
			in:      Input{Branches: texasBranches(), ConfigLat: cfgLat, ConfigLng: cfgLng, Stops: texasStops()},
			wantLat: 30.25, wantLng: -97.75, wantSource: SourceConfig,
		},
		{
			name:    "an empty branch id is not a branch",
			in:      Input{BranchIDs: []string{""}, Branches: texasBranches(), Stops: texasStops()},
			wantLat: 32.80, wantLng: -97.00, wantSource: SourceCentroid,
		},
		{
			// Inactive branches are not returned by GableLBM at all.
			name: "branch wanted but unknown falls back and says why",
			in: Input{BranchIDs: []string{closedYardID}, Branches: texasBranches(),
				ConfigLat: cfgLat, ConfigLng: cfgLng, Stops: texasStops()},
			wantLat: 30.25, wantLng: -97.75, wantSource: SourceConfig,
			wantNote: "not in GableLBM's active branch list",
		},
		{
			// nil must never be read as 0,0 — a route rooted in the Gulf of
			// Guinea would look like an answer.
			name: "branch wanted but never geocoded falls back and says why",
			in: Input{BranchIDs: []string{planoYardID}, Branches: texasBranches(),
				ConfigLat: cfgLat, ConfigLng: cfgLng, Stops: texasStops()},
			wantLat: 30.25, wantLng: -97.75, wantSource: SourceConfig,
			wantNote: "has no coordinates",
		},
		{
			// The caller could not fetch the branch list. To the ladder that is
			// indistinguishable from an unknown branch; the caller rewrites the
			// first half of the sentence (see FallbackPhrase).
			name: "branch wanted but the list is missing falls back and says why",
			in: Input{BranchIDs: []string{dallasYardID}, Branches: nil,
				ConfigLat: cfgLat, ConfigLng: cfgLng, Stops: texasStops()},
			wantLat: 30.25, wantLng: -97.75, wantSource: SourceConfig,
			wantNote: "not in GableLBM's active branch list",
		},
		{
			// Data, not boot config — so it cannot be a fatal error, but it is
			// refused just as firmly as config.loadDepot refuses it.
			name: "out-of-range branch coordinates are refused, not half-applied",
			in: Input{BranchIDs: []string{dallasYardID},
				Branches: []gable.Location{{ID: dallasYardID, Name: "Dallas Yard",
					Latitude: fptr(132.7767), Longitude: fptr(-96.7970)}},
				Stops: texasStops()},
			wantLat: 32.80, wantLng: -97.00, wantSource: SourceCentroid,
			wantNote: "out-of-range",
		},
		{
			// Picking either yard would silently root half the stops wrong;
			// splitting the run is Phase 2.
			name: "two branches on one run fall back and name both yards",
			in: Input{BranchIDs: []string{dallasYardID, fortWorthYardID}, Branches: texasBranches(),
				ConfigLat: cfgLat, ConfigLng: cfgLng, Stops: texasStops()},
			wantLat: 30.25, wantLng: -97.75, wantSource: SourceConfig,
			wantNote: "2 different branches",
		},
		{
			// Nothing left below the branch rung: the note must not pretend
			// otherwise.
			name:       "a declined branch with no fallback reports NONE and says so",
			in:         Input{BranchIDs: []string{closedYardID}, Branches: texasBranches()},
			wantLat:    0,
			wantLng:    0,
			wantSource: SourceNone,
			wantNote:   "nothing else to root on",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lat, lng, src, note := Resolve(tc.in)
			if math.Abs(lat-tc.wantLat) > 1e-6 || math.Abs(lng-tc.wantLng) > 1e-6 {
				t.Fatalf("depot = (%v, %v), want (%v, %v)", lat, lng, tc.wantLat, tc.wantLng)
			}
			if src != tc.wantSource {
				t.Fatalf("depot source = %q, want %q", src, tc.wantSource)
			}
			switch {
			case tc.wantNote == "" && note != "":
				t.Fatalf("nothing was declined, but a note was produced: %q", note)
			case tc.wantNote != "" && !strings.Contains(note, tc.wantNote):
				t.Fatalf("note = %q, want it to explain %q", note, tc.wantNote)
			}
			if lat == retiredHardcodedLat || lng == retiredHardcodedLng {
				t.Fatalf("rooted at the retired hardcoded Kelowna depot (%v, %v)", lat, lng)
			}
		})
	}
}

// TestNoteNamesTheFallbackItActuallyUsed: the second half of every note must
// name where the run really ended up. Support reads this sentence to explain a
// route, so it may not flatter the result.
func TestNoteNamesTheFallbackItActuallyUsed(t *testing.T) {
	_, _, _, toConfig := Resolve(Input{BranchIDs: []string{planoYardID}, Branches: texasBranches(),
		ConfigLat: fptr(30.25), ConfigLng: fptr(-97.75), Stops: texasStops()})
	if !strings.Contains(toConfig, "Plano Yard") || !strings.Contains(toConfig, "configured depot") {
		t.Fatalf("note = %q, want it to name the yard and the configured-depot fallback", toConfig)
	}

	_, _, _, toCentroid := Resolve(Input{BranchIDs: []string{planoYardID}, Branches: texasBranches(),
		Stops: texasStops()})
	if !strings.Contains(toCentroid, "centroid of the day's stops") {
		t.Fatalf("note = %q, want it to name the centroid fallback", toCentroid)
	}
}

// TestUngeocodedBranchIsNeverReadAsZeroZero pins the single most damaging
// misreading of GableLBM's payload.
func TestUngeocodedBranchIsNeverReadAsZeroZero(t *testing.T) {
	lat, lng, src, _ := Resolve(Input{
		BranchIDs: []string{planoYardID}, Branches: texasBranches(), Stops: texasStops(),
	})
	if src == SourceBranch || (lat == 0 && lng == 0) {
		t.Fatalf("an ungeocoded branch was taken as a real origin: (%v, %v) source %q", lat, lng, src)
	}
}

// TestCentroidIsRoundedToCoordinatePrecision keeps a derived origin at ~11 cm
// precision instead of a 17-digit float that reads as false accuracy.
func TestCentroidIsRoundedToCoordinatePrecision(t *testing.T) {
	lat, lng, src, _ := Resolve(Input{Stops: []Point{
		{Lat: 1.0000001, Lng: 2.0000001},
		{Lat: 1.0000002, Lng: 2.0000002},
		{Lat: 1.0000004, Lng: 2.0000004},
	}})
	if src != SourceCentroid {
		t.Fatalf("source = %q, want %q", src, SourceCentroid)
	}
	if lat != 1.0 || lng != 2.0 {
		t.Fatalf("centroid = (%v, %v), want it rounded to (1, 2)", lat, lng)
	}
}

// TestValidCoords refuses what config.loadDepot refuses, and for the same
// reason: a coordinate that is not on Earth is not a fallback, it is a bug with
// a decimal point.
func TestValidCoords(t *testing.T) {
	cases := []struct {
		lat, lng float64
		want     bool
	}{
		{32.7767, -96.7970, true},
		{-90, -180, true},
		{90, 180, true},
		{90.0001, 0, false},
		{0, 180.0001, false},
		{math.NaN(), 0, false},
		{0, math.Inf(1), false},
	}
	for _, c := range cases {
		if got := ValidCoords(c.lat, c.lng); got != c.want {
			t.Errorf("ValidCoords(%v, %v) = %v, want %v", c.lat, c.lng, got, c.want)
		}
	}
}
