// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

// The depot the module used to hardcode — one specific dealer's yard in
// Kelowna BC. No code path may ever produce it again for a dealer that did not
// ask for it: every route, distance, duration and shift check is rooted there.
const (
	retiredGableDepotLat = 49.8863
	retiredGableDepotLng = -119.4666
)

// The dealer's yards, as GableLBM reports them on
// GET /api/integration/locations. Plano has never been geocoded: GableLBM omits
// the coordinate keys entirely, so they arrive here as nil.
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

// texasOrders is the pre-branch fixture: orders with no branch_id at all, as a
// GableLBM that predates migration 062 reports them. Keeping it branchless is
// deliberate — the tests below that use it assert the old chain still behaves
// exactly as it did.
func texasOrders() []gable.Order {
	return []gable.Order{
		{ID: "o1", CustomerName: "Lone Star Framing", Latitude: fptr(32.70), Longitude: fptr(-96.90),
			Lines: []gable.OrderLine{{ProductID: "p1", SKU: "2x4-8", Quantity: 10, WeightLbs: 10}}},
		{ID: "o2", CustomerName: "Bluebonnet Homes", Latitude: fptr(32.90), Longitude: fptr(-97.10),
			Lines: []gable.OrderLine{{ProductID: "p1", SKU: "2x4-8", Quantity: 10, WeightLbs: 10}}},
	}
}

// shippingFrom stamps a yard onto each order in turn, cycling through the ids
// given: one id means "this whole run leaves from one yard", two means the run
// spans two.
func shippingFrom(in []gable.Order, branchIDs ...string) []gable.Order {
	out := make([]gable.Order, len(in))
	copy(out, in)
	for i := range out {
		out[i].BranchID = branchIDs[i%len(branchIDs)]
	}
	return out
}

// TestResolveDepotPrecedence pins the four-level precedence: the ingest request
// wins, then the branch every order on the run ships from, then this install's
// configured yard, then the centroid of the day's own stops. There is no
// built-in coordinate at any level, and every step that declines the branch
// says why.
func TestResolveDepotPrecedence(t *testing.T) {
	analyses := []OrderAnalysis{
		{OrderID: "a", Lat: fptr(32.70), Lng: fptr(-96.90), Routable: true},
		{OrderID: "b", Lat: fptr(32.90), Lng: fptr(-97.10), Routable: true},
		{OrderID: "c", Lat: nil, Lng: nil, Routable: false}, // must not skew the centroid
	}
	// withBranch is the analyses-side twin of shippingFrom.
	withBranch := func(in []OrderAnalysis, branchIDs ...string) []OrderAnalysis {
		out := make([]OrderAnalysis, len(in))
		copy(out, in)
		for i := range out {
			out[i].BranchID = branchIDs[i%len(branchIDs)]
		}
		return out
	}
	cfgDepot := Config{DepotLat: fptr(30.25), DepotLng: fptr(-97.75)}

	cases := []struct {
		name             string
		req              IngestRequest
		cfg              Config
		analyses         []OrderAnalysis
		branches         []gable.Location
		wantLat, wantLng float64
		wantSource       string
		// wantNote is a substring the recorded reason must contain; empty means
		// the note must be empty, because nothing was declined.
		wantNote string
	}{
		{
			name: "request depot wins over config and stops",
			req:  IngestRequest{Date: "2026-06-26", DepotLat: fptr(29.76), DepotLng: fptr(-95.36)},
			cfg:  cfgDepot, analyses: analyses,
			wantLat: 29.76, wantLng: -95.36, wantSource: DepotSourceRequest,
		},
		{
			name: "configured depot wins over the centroid",
			req:  IngestRequest{Date: "2026-06-26"},
			cfg:  cfgDepot, analyses: analyses,
			wantLat: 30.25, wantLng: -97.75, wantSource: DepotSourceConfig,
		},
		{
			name: "centroid of routable stops when nothing is configured",
			req:  IngestRequest{Date: "2026-06-26"},
			cfg:  Config{}, analyses: analyses,
			wantLat: 32.80, wantLng: -97.00, wantSource: DepotSourceCentroid,
		},
		{
			name: "half-configured depot is ignored, not half-applied",
			req:  IngestRequest{Date: "2026-06-26", DepotLat: fptr(29.76)},
			cfg:  Config{DepotLat: fptr(30.25)}, analyses: analyses,
			wantLat: 32.80, wantLng: -97.00, wantSource: DepotSourceCentroid,
		},
		{
			name: "nothing to root on",
			req:  IngestRequest{Date: "2026-06-26"},
			cfg:  Config{}, analyses: []OrderAnalysis{{OrderID: "z", Routable: false}},
			wantLat: 0, wantLng: 0, wantSource: DepotSourceNone,
		},
		{
			// The whole point of Phase 1: the yard the load leaves from beats
			// the one global coordinate this install happens to be configured
			// with. A dealer with two yards no longer roots half its day wrong.
			name: "the run's branch wins over the configured yard",
			req:  IngestRequest{Date: "2026-06-26"},
			cfg:  cfgDepot, analyses: withBranch(analyses, dallasYardID), branches: texasBranches(),
			wantLat: dallasYardLat, wantLng: dallasYardLng, wantSource: DepotSourceBranch,
		},
		{
			name: "an explicit request depot still outranks the branch",
			req:  IngestRequest{Date: "2026-06-26", DepotLat: fptr(29.76), DepotLng: fptr(-95.36)},
			cfg:  cfgDepot, analyses: withBranch(analyses, dallasYardID), branches: texasBranches(),
			wantLat: 29.76, wantLng: -95.36, wantSource: DepotSourceRequest,
		},
		{
			// Two yards, one plan. Picking either would silently root half the
			// stops at the wrong place; splitting the run is Phase 2.
			name: "orders spanning two branches fall back and say why",
			req:  IngestRequest{Date: "2026-06-26"},
			cfg:  cfgDepot, analyses: withBranch(analyses, dallasYardID, fortWorthYardID), branches: texasBranches(),
			wantLat: 30.25, wantLng: -97.75, wantSource: DepotSourceConfig,
			wantNote: "2 different branches",
		},
		{
			name: "a branch that has never been geocoded falls back and says why",
			req:  IngestRequest{Date: "2026-06-26"},
			cfg:  cfgDepot, analyses: withBranch(analyses, planoYardID), branches: texasBranches(),
			wantLat: 30.25, wantLng: -97.75, wantSource: DepotSourceConfig,
			wantNote: "has no coordinates",
		},
		{
			// Inactive branches are not returned by GableLBM at all.
			name: "an unknown branch falls back and says why",
			req:  IngestRequest{Date: "2026-06-26"},
			cfg:  cfgDepot, analyses: withBranch(analyses, closedYardID), branches: texasBranches(),
			wantLat: 30.25, wantLng: -97.75, wantSource: DepotSourceConfig,
			wantNote: "not in GableLBM's active branch list",
		},
		{
			// Data, not boot config — so it cannot be a fatal error, but it is
			// refused just as firmly as config.loadDepot refuses it.
			name:     "out-of-range branch coordinates are refused, not half-applied",
			req:      IngestRequest{Date: "2026-06-26"},
			cfg:      Config{},
			analyses: withBranch(analyses, dallasYardID),
			branches: []gable.Location{{ID: dallasYardID, Name: "Dallas Yard",
				Latitude: fptr(132.7767), Longitude: fptr(-96.7970)}},
			wantLat: 32.80, wantLng: -97.00, wantSource: DepotSourceCentroid,
			wantNote: "out-of-range",
		},
		{
			// Nothing left below the branch step: the note must not pretend
			// otherwise.
			name: "a declined branch with no fallback reports NONE and says so",
			req:  IngestRequest{Date: "2026-06-26"},
			cfg:  Config{},
			analyses: []OrderAnalysis{
				{OrderID: "a", BranchID: dallasYardID, Routable: false},
				{OrderID: "b", BranchID: fortWorthYardID, Routable: false},
			},
			branches: texasBranches(),
			wantLat:  0, wantLng: 0, wantSource: DepotSourceNone,
			wantNote: "nothing else to root on",
		},
		{
			// A GableLBM that predates orders.branch_id. Not an error, not a
			// note — exactly the behaviour of the day before this change.
			name: "orders with no branch id behave exactly as before, silently",
			req:  IngestRequest{Date: "2026-06-26"},
			cfg:  cfgDepot, analyses: analyses, branches: texasBranches(),
			wantLat: 30.25, wantLng: -97.75, wantSource: DepotSourceConfig,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lat, lng, src, note := resolveDepot(tc.req, tc.cfg, tc.analyses, tc.branches)
			if math.Abs(lat-tc.wantLat) > 1e-6 || math.Abs(lng-tc.wantLng) > 1e-6 {
				t.Fatalf("depot = (%v, %v), want (%v, %v)", lat, lng, tc.wantLat, tc.wantLng)
			}
			if src != tc.wantSource {
				t.Fatalf("depot source = %q, want %q", src, tc.wantSource)
			}
			switch {
			case tc.wantNote == "" && note != "":
				t.Fatalf("nothing was declined, but the plan carries a note: %q", note)
			case tc.wantNote != "" && !strings.Contains(note, tc.wantNote):
				t.Fatalf("note = %q, want it to explain %q", note, tc.wantNote)
			}
		})
	}
}

// TestIngestUsesConfiguredDepot verifies the configured yard reaches the
// persisted plan (the dealer's own coordinates, not a code constant).
func TestIngestUsesConfiguredDepot(t *testing.T) {
	store := newFakePlanStore()
	svc := newTestService(store, &fakeGable{orders: texasOrders()},
		Config{DepotLat: fptr(32.7767), DepotLng: fptr(-96.7970)})

	p, err := svc.Ingest(context.Background(), IngestRequest{Date: "2026-06-26"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if p.DepotLat != 32.7767 || p.DepotLng != -96.7970 {
		t.Fatalf("plan depot = (%v, %v), want the configured yard (32.7767, -96.7970)", p.DepotLat, p.DepotLng)
	}
	if p.DepotSource != DepotSourceConfig {
		t.Fatalf("depot source = %q, want %q", p.DepotSource, DepotSourceConfig)
	}
	if got := store.stored(p.ID); got.DepotLat != p.DepotLat || got.DepotSource != DepotSourceConfig {
		t.Fatalf("depot did not persist: %+v", got)
	}
}

// TestIngestUnconfiguredDepotUsesStopCentroidNotAForeignYard is the regression
// guard for the hardcoded-depot blocker: an unconfigured install must root a
// Texas dealer's plan in Texas — near its own stops — never at the Kelowna BC
// yard the module used to hardcode ~1,700 miles away.
func TestIngestUnconfiguredDepotUsesStopCentroidNotAForeignYard(t *testing.T) {
	store := newFakePlanStore()
	svc := newTestService(store, &fakeGable{orders: texasOrders()}, Config{})

	p, err := svc.Ingest(context.Background(), IngestRequest{Date: "2026-06-26"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if p.DepotLat == retiredGableDepotLat || p.DepotLng == retiredGableDepotLng {
		t.Fatalf("plan rooted at the retired hardcoded Kelowna depot (%v, %v)", p.DepotLat, p.DepotLng)
	}
	if p.DepotSource != DepotSourceCentroid {
		t.Fatalf("depot source = %q, want %q", p.DepotSource, DepotSourceCentroid)
	}
	// Centroid of the two Texas stops.
	if math.Abs(p.DepotLat-32.80) > 1e-6 || math.Abs(p.DepotLng-(-97.00)) > 1e-6 {
		t.Fatalf("depot = (%v, %v), want the stop centroid (32.80, -97.00)", p.DepotLat, p.DepotLng)
	}
}

// TestIngestRequestDepotOverridesConfig verifies a per-run override still wins.
func TestIngestRequestDepotOverridesConfig(t *testing.T) {
	store := newFakePlanStore()
	svc := newTestService(store, &fakeGable{orders: texasOrders()},
		Config{DepotLat: fptr(32.7767), DepotLng: fptr(-96.7970)})

	p, err := svc.Ingest(context.Background(), IngestRequest{
		Date: "2026-06-26", DepotLat: fptr(29.7604), DepotLng: fptr(-95.3698),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if p.DepotLat != 29.7604 || p.DepotLng != -95.3698 || p.DepotSource != DepotSourceRequest {
		t.Fatalf("request depot must win: got (%v, %v) source %q", p.DepotLat, p.DepotLng, p.DepotSource)
	}
}

// --- End to end: the branch depot on a real ingest ---------------------------

// TestIngestUsesTheBranchTheOrdersShipFrom is the payoff. This install is
// configured with one global depot in Austin, but every order on the day leaves
// the Dallas yard, and GableLBM knows that per order. The plan must root in
// Dallas — and must say BRANCH, so support can see it did.
func TestIngestUsesTheBranchTheOrdersShipFrom(t *testing.T) {
	store := newFakePlanStore()
	svc := newTestService(store, &fakeGable{
		orders:    shippingFrom(texasOrders(), dallasYardID),
		locations: texasBranches(),
	}, Config{DepotLat: fptr(30.2672), DepotLng: fptr(-97.7431)}) // Austin

	p, err := svc.Ingest(context.Background(), IngestRequest{Date: "2026-06-26"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if math.Abs(p.DepotLat-dallasYardLat) > 1e-6 || math.Abs(p.DepotLng-dallasYardLng) > 1e-6 {
		t.Fatalf("plan depot = (%v, %v), want the Dallas yard (%v, %v)",
			p.DepotLat, p.DepotLng, dallasYardLat, dallasYardLng)
	}
	if p.DepotSource != DepotSourceBranch {
		t.Fatalf("depot source = %q, want %q", p.DepotSource, DepotSourceBranch)
	}
	if p.DepotNote != "" {
		t.Fatalf("nothing was declined, but the plan carries a note: %q", p.DepotNote)
	}
	if p.Orders[0].BranchID != dallasYardID {
		t.Errorf("order analysis lost its branch: %q", p.Orders[0].BranchID)
	}
	got := store.stored(p.ID)
	if got.DepotSource != DepotSourceBranch || math.Abs(got.DepotLat-dallasYardLat) > 1e-6 {
		t.Fatalf("branch depot did not persist: %+v", got)
	}
}

// TestIngestSpanningTwoBranchesFallsBackAndSaysWhy: a dealer shipping from two
// yards on one day must NOT have one of them silently chosen. Splitting the run
// per branch is Phase 2; until then the plan falls back down the chain and
// carries a sentence naming both yards, because a dispatcher looking at a route
// rooted in Austin deserves to know why it is not rooted at their yard.
func TestIngestSpanningTwoBranchesFallsBackAndSaysWhy(t *testing.T) {
	store := newFakePlanStore()
	svc := newTestService(store, &fakeGable{
		orders:    shippingFrom(texasOrders(), dallasYardID, fortWorthYardID),
		locations: texasBranches(),
	}, Config{DepotLat: fptr(30.2672), DepotLng: fptr(-97.7431)})

	p, err := svc.Ingest(context.Background(), IngestRequest{Date: "2026-06-26"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if p.DepotSource != DepotSourceConfig {
		t.Fatalf("depot source = %q, want %q — a multi-branch run must not claim a branch origin",
			p.DepotSource, DepotSourceConfig)
	}
	if math.Abs(p.DepotLat-dallasYardLat) < 1e-6 || math.Abs(p.DepotLat-32.7555) < 1e-6 {
		t.Fatalf("a yard was silently picked out of two: depot = (%v, %v)", p.DepotLat, p.DepotLng)
	}
	for _, want := range []string{"Dallas Yard", "Fort Worth Yard", "configured depot"} {
		if !strings.Contains(p.DepotNote, want) {
			t.Errorf("note = %q, want it to mention %q", p.DepotNote, want)
		}
	}
	if got := store.stored(p.ID); got.DepotNote != p.DepotNote || got.DepotSource != DepotSourceConfig {
		t.Fatalf("the reason did not persist: %+v", got)
	}
}

// TestIngestUngeocodedBranchFallsBackAndSaysWhy: the Plano yard exists but has
// never been geocoded, so GableLBM omits its coordinates. nil must never be
// read as 0,0 — a plan rooted in the Gulf of Guinea would look like an answer.
func TestIngestUngeocodedBranchFallsBackAndSaysWhy(t *testing.T) {
	store := newFakePlanStore()
	svc := newTestService(store, &fakeGable{
		orders:    shippingFrom(texasOrders(), planoYardID),
		locations: texasBranches(),
	}, Config{DepotLat: fptr(30.2672), DepotLng: fptr(-97.7431)})

	p, err := svc.Ingest(context.Background(), IngestRequest{Date: "2026-06-26"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if p.DepotLat == 0 && p.DepotLng == 0 {
		t.Fatal("an ungeocoded branch was taken as (0,0)")
	}
	if p.DepotSource != DepotSourceConfig {
		t.Fatalf("depot source = %q, want %q", p.DepotSource, DepotSourceConfig)
	}
	if !strings.Contains(p.DepotNote, "Plano Yard") || !strings.Contains(p.DepotNote, "never been geocoded") {
		t.Fatalf("note = %q, want it to name the yard and say it was never geocoded", p.DepotNote)
	}
}

// TestIngestSurvivesAGableWithoutTheLocationsEndpoint: AI_LM and GableLBM
// deploy separately. An ERP that has not shipped /api/integration/locations yet
// answers 404, and that must cost this run its branch depot — not its plan.
func TestIngestSurvivesAGableWithoutTheLocationsEndpoint(t *testing.T) {
	store := newFakePlanStore()
	svc := newTestService(store, &fakeGable{
		orders: shippingFrom(texasOrders(), dallasYardID),
		locErr: errors.New("gable GET /api/integration/locations: status 404"),
	}, Config{DepotLat: fptr(30.2672), DepotLng: fptr(-97.7431)})

	p, err := svc.Ingest(context.Background(), IngestRequest{Date: "2026-06-26"})
	if err != nil {
		t.Fatalf("a GableLBM without the locations endpoint must not fail the ingest: %v", err)
	}
	if p.DepotSource != DepotSourceConfig {
		t.Fatalf("depot source = %q, want %q", p.DepotSource, DepotSourceConfig)
	}
	// The orders DID name a yard, so the note must blame the lookup, not
	// pretend the branch was unknown to GableLBM.
	if !strings.Contains(p.DepotNote, "could not read GableLBM's branches") ||
		!strings.Contains(p.DepotNote, "404") {
		t.Fatalf("note = %q, want it to report the failed branch lookup", p.DepotNote)
	}
}

// TestIngestWithoutBranchIDsIsSilent guards the other deployment direction: an
// ERP that predates orders.branch_id must behave exactly as it did before this
// change, with no note attached to every plan it produces.
func TestIngestWithoutBranchIDsIsSilent(t *testing.T) {
	store := newFakePlanStore()
	svc := newTestService(store, &fakeGable{
		orders: texasOrders(), // no branch_id at all
		locErr: errors.New("gable GET /api/integration/locations: status 404"),
	}, Config{DepotLat: fptr(32.7767), DepotLng: fptr(-96.7970)})

	p, err := svc.Ingest(context.Background(), IngestRequest{Date: "2026-06-26"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if p.DepotSource != DepotSourceConfig || p.DepotNote != "" {
		t.Fatalf("pre-branch ERP changed behaviour: source %q note %q", p.DepotSource, p.DepotNote)
	}
}

// TestRequestDepotSkipsTheBranchLookupEntirely: an explicit override is the
// operator's answer and nothing may outrank it, so the ERP is not even asked —
// a run with a request depot must succeed on an ERP whose branch endpoint is
// down.
func TestRequestDepotSkipsTheBranchLookupEntirely(t *testing.T) {
	store := newFakePlanStore()
	svc := newTestService(store, &fakeGable{
		orders: shippingFrom(texasOrders(), dallasYardID),
		locErr: errors.New("gable GET /api/integration/locations: status 500"),
	}, Config{})

	p, err := svc.Ingest(context.Background(), IngestRequest{
		Date: "2026-06-26", DepotLat: fptr(29.7604), DepotLng: fptr(-95.3698),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if p.DepotSource != DepotSourceRequest || p.DepotLat != 29.7604 || p.DepotNote != "" {
		t.Fatalf("request depot must win outright: (%v, %v) source %q note %q",
			p.DepotLat, p.DepotLng, p.DepotSource, p.DepotNote)
	}
}

// TestDepotNoteSurvivesThePayloadRoundTrip pins the JSONB half: DepotNote lives
// in the plan payload document, and a field added to only one of
// marshalPayload/unmarshalPayload round-trips as an empty string in production
// while every in-memory test still passes.
func TestDepotNoteSurvivesThePayloadRoundTrip(t *testing.T) {
	r := &Repository{}
	in := &Plan{
		DepotLat: dallasYardLat, DepotLng: dallasYardLng,
		DepotSource: DepotSourceBranch,
		DepotNote:   "orders ship from 2 different branches",
		Orders:      []OrderAnalysis{{OrderID: "o1", BranchID: dallasYardID}},
	}
	raw, err := r.marshalPayload(in)
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	var out Plan
	if err := r.unmarshalPayload(raw, &out); err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if out.DepotSource != in.DepotSource || out.DepotNote != in.DepotNote {
		t.Fatalf("depot provenance did not round-trip: source %q note %q", out.DepotSource, out.DepotNote)
	}
	if len(out.Orders) != 1 || out.Orders[0].BranchID != dallasYardID {
		t.Fatalf("order branch did not round-trip: %+v", out.Orders)
	}
}
