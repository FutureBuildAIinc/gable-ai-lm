// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/depot"
)

// The depot ladder writes a sentence for a dispatcher — "your Plano yard has
// never been geocoded, so this run is rooted at Dallas instead" — and until
// this change only one of the two surfaces that plan a day ever showed it to
// anyone. workflow.Plan carried it to the client; routing.Plan logged it and
// dropped it, so a dispatcher calling POST /api/v1/routing/plan was never told
// their route had been rooted at a yard their load does not leave from. The
// warning existed and did not reach the person it was written for.
//
// route_plans is column-backed and there is still no column for it, so the fix
// is response-only: the two values ride the Plan struct out of Plan() and are
// gone on any later read. The tests below pin BOTH halves — that the CREATE
// response carries the note, and that the read path returns an empty one and
// that this is the documented, intended answer rather than a bug to be
// "helpfully" patched by re-resolving.

// columnBackedStore models route_plans as it actually is. Save keeps exactly
// the fields Repository.Save names in its INSERT and Get returns exactly the
// fields Repository.Get names in its SELECT; anything else the Plan struct
// carries does not survive the round trip. fakeStore, which hands back the very
// pointer it was given, cannot show that — it would report a note on read that
// Postgres could never have returned.
//
// If a migration ever adds depot_source/depot_note columns, this fake and
// TestGetOfAStoredPlanHasNoDepotNote are what should change with it.
type columnBackedStore struct{ rows map[string]Plan }

func (s *columnBackedStore) Save(_ context.Context, p *Plan) error {
	if s.rows == nil {
		s.rows = make(map[string]Plan)
	}
	p.ID = fmt.Sprintf("plan-%d", len(s.rows)+1)
	p.CreatedAt = time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	p.UpdatedAt = p.CreatedAt
	s.rows[p.ID] = Plan{
		ID:               p.ID,
		PlanDate:         p.PlanDate,
		GableBranchID:    p.GableBranchID,
		GableVehicleID:   p.GableVehicleID,
		Stops:            p.Stops,
		Loads:            p.Loads,
		TotalDistanceMi:  p.TotalDistanceMi,
		TotalDurationMin: p.TotalDurationMin,
		Status:           p.Status,
		CreatedAt:        p.CreatedAt,
		UpdatedAt:        p.UpdatedAt,
	}
	return nil
}

func (s *columnBackedStore) Get(_ context.Context, id string) (*Plan, error) {
	row, ok := s.rows[id]
	if !ok {
		return nil, ErrNotFound
	}
	if row.UnassignedStops == nil {
		row.UnassignedStops = []Stop{} // Repository.Get normalizes this; so must the fake.
	}
	return &row, nil
}

func (s *columnBackedStore) UpdateStatus(context.Context, string, string) error { return nil }

// planStored runs a plan against a store that persists only what has a column,
// and hands back both the CREATE response and the store it was written to.
func planStored(t *testing.T, g *fakeGable, cfg Config, req PlanRequest) (*Service, *Plan) {
	t.Helper()
	svc := NewService(&columnBackedStore{}, g, g, g, g, g, cfg)
	plan, err := svc.Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return svc, plan
}

// TestPlanResponseCarriesTheDepotNote is the defect itself. The request names
// Plano, which GableLBM has never geocoded; the orders all ship from Dallas, so
// the run roots there (BRANCH outranks CONFIG, and the branch that failed is
// not the branch being asked about). The dispatcher asked for one yard and got
// another, and the response has to say so.
func TestPlanResponseCarriesTheDepotNote(t *testing.T) {
	g := &fakeGable{
		orders:    shippingFrom(routingOrders(), dallasYardID),
		locations: routingBranches(),
	}
	cfg := Config{DepotLat: fptr(austinConfigLat), DepotLng: fptr(austinConfigLng)}
	req := PlanRequest{Date: "2026-06-26", BranchID: sptr(planoYardID)}

	_, plan := planStored(t, g, cfg, req)

	if plan.DepotSource != depot.SourceBranch {
		t.Fatalf("depot source on the create response = %q, want %q — the run rooted at the orders' own yard",
			plan.DepotSource, depot.SourceBranch)
	}
	if plan.DepotNote == "" {
		t.Fatal("the run was rooted at a yard the dispatcher did not ask for and the response says nothing")
	}
	for _, want := range []string{"Plano Yard", "never been geocoded", "Dallas Yard"} {
		if !strings.Contains(plan.DepotNote, want) {
			t.Errorf("note = %q, want it to mention %q", plan.DepotNote, want)
		}
	}
}

// TestPlanResponseNoteMatchesTheLoggedNote pins the property that makes the log
// a usable durable copy of a response-only field: support reading the log line
// and the dispatcher reading the response must be looking at the same sentence,
// not two paraphrases of one event.
func TestPlanResponseNoteMatchesTheLoggedNote(t *testing.T) {
	cfg := Config{DepotLat: fptr(austinConfigLat), DepotLng: fptr(austinConfigLng)}
	req := PlanRequest{Date: "2026-06-26", BranchID: sptr(planoYardID)}
	orders := shippingFrom(routingOrders(), dallasYardID)

	// resolveDepot is the single source of both copies; ask it directly for what
	// Plan() would have logged.
	_, _, wantSource, wantNote := resolveWith(t,
		&fakeGable{orders: orders, locations: routingBranches()}, cfg, req)

	_, plan := planStored(t, &fakeGable{orders: orders, locations: routingBranches()}, cfg, req)

	if plan.DepotSource != wantSource || plan.DepotNote != wantNote {
		t.Fatalf("response carries source %q note %q; the log line carries source %q note %q",
			plan.DepotSource, plan.DepotNote, wantSource, wantNote)
	}
}

// TestCleanPlanCarriesNoDepotNote: nothing was declined, so there is nothing to
// warn about. A note on every plan is a note nobody reads, and it would make
// the presence of a note stop meaning anything.
func TestCleanPlanCarriesNoDepotNote(t *testing.T) {
	g := &fakeGable{
		orders:    shippingFrom(routingOrders(), dallasYardID),
		locations: routingBranches(),
	}
	cfg := Config{DepotLat: fptr(austinConfigLat), DepotLng: fptr(austinConfigLng)}

	_, plan := planStored(t, g, cfg, PlanRequest{Date: "2026-06-26", BranchID: sptr(dallasYardID)})

	if plan.DepotSource != depot.SourceBranch {
		t.Fatalf("depot source = %q, want %q", plan.DepotSource, depot.SourceBranch)
	}
	if plan.DepotNote != "" {
		t.Fatalf("the request, the orders and GableLBM all agree, but the plan carries a note: %q", plan.DepotNote)
	}

	// And the field is absent from the payload rather than present-and-empty.
	body, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "depot_note") {
		t.Fatalf("happy-path response ships a depot_note key: %s", body)
	}
	if !strings.Contains(string(body), `"depot_source":"BRANCH"`) {
		t.Fatalf("response lost its depot_source: %s", body)
	}
}

// TestGetOfAStoredPlanHasNoDepotNote asserts the documented limitation, so that
// it is a decision on the record rather than a silence.
//
// route_plans has no depot_source/depot_note column, so a plan re-read from the
// store carries neither. That empty note means "this plan's provenance was
// never stored" — NOT "no fallback happened", which is what the identical empty
// string means on the create path. Nothing may treat a read-path blank as an
// all-clear, and nothing may fill it in by re-resolving: the branch may have
// been geocoded, or DEPOT_LAT changed, since the plan was built, so a
// re-resolve would answer today's question about an old plan and present the
// answer as history. Adding the two columns in a migration is the only thing
// that changes this.
func TestGetOfAStoredPlanHasNoDepotNote(t *testing.T) {
	g := &fakeGable{
		orders:    shippingFrom(routingOrders(), dallasYardID),
		locations: routingBranches(),
	}
	cfg := Config{DepotLat: fptr(austinConfigLat), DepotLng: fptr(austinConfigLng)}

	svc, created := planStored(t, g, cfg, PlanRequest{Date: "2026-06-26", BranchID: sptr(planoYardID)})
	if created.DepotNote == "" || created.DepotSource == "" {
		t.Fatalf("precondition: the create response should carry provenance, got source %q note %q",
			created.DepotSource, created.DepotNote)
	}

	reread, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if reread.DepotSource != "" || reread.DepotNote != "" {
		t.Fatalf("a re-read plan reported provenance no column holds: source %q note %q — "+
			"either a migration landed (update this test with it) or the read path is inventing values",
			reread.DepotSource, reread.DepotNote)
	}
	// Everything that DOES have a column still round-trips, so the test is
	// pinning the missing columns and not a broken store.
	if reread.ID != created.ID || reread.Status != created.Status || len(reread.Loads) != len(created.Loads) {
		t.Fatalf("the column-backed half of the plan did not survive the round trip: %+v", reread)
	}

	body, err := json.Marshal(reread)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "depot_note") || strings.Contains(string(body), "depot_source") {
		t.Fatalf("read-path payload claims provenance it does not have: %s", body)
	}
}
