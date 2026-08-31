// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// This file covers the one live-route path the state machine could not see.
//
// gateTransition keys on p.Status and p.LiveRoutes — both properties of the
// plan being changed. Ingest changes no plan: it mints a NEW one for a date and
// walks away from whatever was already holding it. The new plan is born
// ANALYZED with an empty ledger, and the recall machinery is scoped to that
// ledger, so nothing in the system could ever withdraw the routes the previous
// plan had left on the dealer's dispatch board.
//
// Reproduced before the fix: re-ingest on a date with 5 live routes was
// ALLOWED (new plan status=ANALYZED live_routes=0), the whole sequence made 0
// recall calls, and two trucks were left holding live routes for a run that no
// longer existed. That is the same sentence the state-machine work claims to
// have removed, reached through "the day changed, re-run it" — plausibly a more
// common dispatcher action than re-assigning an already-pushed plan.

// livePlanForDate puts n trucks on the fake dispatch board and returns the
// service, the ERP double and the store, so each test below starts from a date
// that is genuinely live.
func livePlanForDate(t *testing.T, n int) (*Service, *fakeGable, *fakePlanStore) {
	t.Helper()
	store := newFakePlanStore(multiTruckPushReadyPlan(n))
	ids := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		ids = append(ids, fmt.Sprintf("v%d", i))
	}
	g := fleetOf(ids...)
	svc := newTestService(store, g, Config{})
	pushPlan(t, svc, store)
	if len(g.pushed) != n {
		t.Fatalf("setup: dispatch board holds %v, want %d routes", g.pushedIDs(), n)
	}
	return svc, g, store
}

// TestReingestingADateWhosePlanIsLiveIsRefused is the defect, stated.
//
// Without the gate this call SUCCEEDS: a second plan appears for the date with
// an empty ledger, and the trucks the first plan put on the board are orphaned
// with nothing left in this system that knows they exist.
func TestReingestingADateWhosePlanIsLiveIsRefused(t *testing.T) {
	svc, g, store := livePlanForDate(t, 2)
	before := store.stored("plan-1")

	_, err := svc.Ingest(context.Background(), IngestRequest{Date: before.PlanDate})
	if err == nil {
		t.Fatal("re-ingesting a date whose routes are live must refuse, not mint a second plan behind the first")
	}
	// The refusal must be the SAME shape as every other live-route refusal on
	// this branch, or the UI has no override prompt to open.
	if !errors.Is(err, ErrPushed) {
		t.Errorf("the refusal must be ErrPushed so the handler answers 423, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "manual approval") {
		t.Errorf("refusal %q must read like the other gates and offer the override", err)
	}
	// It must name the trucks the dispatcher would be stranding, and the plan
	// they belong to — "some other plan has routes out" is not actionable.
	for _, want := range []string{"Flatbed 1", "Flatbed 2", "plan-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q must name %q", err, want)
		}
	}

	// A refusal that created the plan anyway would be the original bug with an
	// error message attached.
	if n := store.count(); n != 1 {
		t.Errorf("the store holds %d plans, want exactly 1 — a refused ingest must create nothing", n)
	}
	if after := store.stored("plan-1"); !reflect.DeepEqual(before, after) {
		t.Errorf("a refused ingest must write NOTHING\n before: %+v\n  after: %+v", before, after)
	}
	if len(g.recalled) != 0 {
		t.Errorf("a refused ingest must not touch the dispatch board, recalled %v", g.recalledIDs())
	}
	if got := g.pushedIDs(); len(got) != 2 {
		t.Errorf("the board must be left exactly as it was, holds %v", got)
	}
}

// recallOrderStore notes how many recalls had already happened when the new
// plan was created. "Recall, THEN create" is an ORDER, and two independent
// end-state assertions cannot tell it from "create, then recall" — which would
// leave a window with a new plan on top of a board still holding the old one's
// routes.
type recallOrderStore struct {
	*fakePlanStore
	g               *fakeGable
	recallsAtCreate int
}

func (s *recallOrderStore) Create(ctx context.Context, p *Plan) error {
	s.recallsAtCreate = len(s.g.recalled)
	return s.fakePlanStore.Create(ctx, p)
}

// TestAnApprovedReingestRecallsEveryRouteTheSupersededPlanLeft pins the other
// half of the decision. Unlike a re-assignment — which keeps the trucks it did
// not drop, because their routes are stale rather than orphaned — a re-ingest
// keeps NOTHING. The new plan is rebuilt from GableLBM's orders as they now
// stand and has no idea the old routes exist, so every one of them must come
// off the board first.
func TestAnApprovedReingestRecallsEveryRouteTheSupersededPlanLeft(t *testing.T) {
	svc, g, base := livePlanForDate(t, 3)
	store := &recallOrderStore{fakePlanStore: base, g: g}
	svc.repo = store

	got, err := svc.Ingest(context.Background(), IngestRequest{
		Date: "2026-06-26", Override: true, ApprovedBy: "dispatcher@dealer.com",
	})
	if err != nil {
		t.Fatalf("an approved re-ingest must proceed: %v", err)
	}

	if ids := g.recalledIDs(); !reflect.DeepEqual(ids, []string{"v1", "v2", "v3"}) {
		t.Errorf("recalled %v, want every truck the superseded plan had live", ids)
	}
	for _, r := range g.recalled {
		// The recall is keyed on (vehicle_id, scheduled_date) upstream; a
		// recall that named the wrong date would withdraw somebody else's run.
		if r.VehicleID == "" || r.ScheduledDate != "2026-06-26" {
			t.Errorf("recall must be keyed on (vehicle_id, scheduled_date), got %+v", r)
		}
		if r.RecalledBy != "dispatcher@dealer.com" {
			t.Errorf("recall must be attributed to the approver, got %q", r.RecalledBy)
		}
		if r.Reason == "" {
			t.Error("recall must say why the route was withdrawn")
		}
	}
	if got := g.pushedIDs(); len(got) != 0 {
		t.Errorf("the dealer's board still holds %v — every superseded route had to come off", got)
	}

	// ...and ONLY then is the new plan created.
	if store.recallsAtCreate != 3 {
		t.Errorf("the new plan was created after %d recall(s), want 3 — the board must be cleared BEFORE a plan is put on top of it",
			store.recallsAtCreate)
	}
	if got.ID == "plan-1" {
		t.Fatal("the re-ingest must produce a NEW plan, not overwrite the superseded one")
	}
	if got.Status != StatusAnalyzed || len(liveRoutes(got)) != 0 {
		t.Errorf("the new plan starts clean: status=%q live=%d", got.Status, len(liveRoutes(got)))
	}
	if n := store.count(); n != 2 {
		t.Errorf("the store holds %d plans, want 2 (the superseded one is kept for audit)", n)
	}

	// The superseded plan carries the whole record: who approved, and which
	// routes were taken off on the strength of it.
	old := store.stored("plan-1")
	if n := len(liveRoutes(old)); n != 0 {
		t.Errorf("the superseded plan still claims %d live route(s) — the ledger must be tombstoned, not left lying", n)
	}
	if len(old.LiveRoutes) != 3 {
		t.Fatalf("recalls are tombstoned, never removed: %+v", old.LiveRoutes)
	}
	for _, r := range old.LiveRoutes {
		if r.RecalledBy != "dispatcher@dealer.com" || r.RecallNote == "" {
			t.Errorf("tombstone %+v must record who approved the recall and why", r)
		}
	}
	if len(old.PushedOverrides) != 1 {
		t.Fatalf("the approval must be recorded on the plan it authorized changing, got %+v", old.PushedOverrides)
	}
	if ov := old.PushedOverrides[0]; ov.Action != actionSupersede || ov.ApprovedBy != "dispatcher@dealer.com" {
		t.Errorf("override %+v must name the action and the approver", ov)
	}
}

// TestAFailedRecallAbortsTheWholeReingest pins the rule the rest of this branch
// already lives by: a half-recalled board with a new plan sitting on top of it
// is worse than refusing outright. If the board cannot be cleared, nothing is
// created and nothing is written — the superseded plan keeps naming every route
// as live, so the retry recalls them all again and converges.
func TestAFailedRecallAbortsTheWholeReingest(t *testing.T) {
	svc, g, store := livePlanForDate(t, 2)
	before := store.stored("plan-1")
	g.recallErr = errors.New("gable POST /api/integration/delivery-routes/recall: status 503: unavailable")

	if _, err := svc.Ingest(context.Background(), IngestRequest{
		Date: "2026-06-26", Override: true, ApprovedBy: "dispatcher@dealer.com",
	}); err == nil {
		t.Fatal("a re-ingest whose recall failed must not succeed")
	}

	if n := store.count(); n != 1 {
		t.Errorf("the store holds %d plans, want 1 — a failed recall must create NOTHING", n)
	}
	after := store.stored("plan-1")
	if !reflect.DeepEqual(before, after) {
		t.Errorf("the superseded plan must be byte-identical\n before: %+v\n  after: %+v", before, after)
	}
	if n := len(liveRoutes(after)); n != 2 {
		t.Errorf("the ledger names %d route(s) as live, want 2 — a recall that never happened must not be tombstoned", n)
	}
	if got := g.pushedIDs(); len(got) != 2 {
		t.Errorf("the dealer's board must be untouched, holds %v", got)
	}

	// And the retry converges once GableLBM is reachable again.
	g.recallErr = nil
	if _, err := svc.Ingest(context.Background(), IngestRequest{
		Date: "2026-06-26", Override: true, ApprovedBy: "dispatcher@dealer.com",
	}); err != nil {
		t.Fatalf("the retry must succeed once the recall endpoint is reachable: %v", err)
	}
	if ids := g.recalledIDs(); !reflect.DeepEqual(ids, []string{"v1", "v2"}) {
		t.Errorf("the retry recalled %v, want both routes", ids)
	}
	if n := store.count(); n != 2 {
		t.Errorf("the retry must create the plan, store holds %d", n)
	}
}

// TestATruckAlreadyOnTheRoadStopsTheReingest is the one recall failure a retry
// cannot fix, and it must arrive as a sentence rather than an upstream error
// code. It reuses the same terminal path the re-assignment gate uses; only the
// clause naming what is blocked differs, because the dispatcher is being told
// about a different operation.
func TestATruckAlreadyOnTheRoadStopsTheReingest(t *testing.T) {
	svc, g, store := livePlanForDate(t, 2)
	before := store.stored("plan-1")
	g.recallDispatched = true

	_, err := svc.Ingest(context.Background(), IngestRequest{
		Date: "2026-06-26", Override: true, ApprovedBy: "dispatcher@dealer.com",
	})
	if err == nil {
		t.Fatal("a truck already on the road must stop the re-ingest")
	}
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("this must be a Refusal so the dispatcher reads it verbatim, got %T: %v", err, err)
	}
	if !strings.Contains(refusal.Msg, "left the yard") || !strings.Contains(refusal.Msg, "re-planned") {
		t.Errorf("refusal %q must say the truck has left the yard and that the date cannot be re-planned around it", refusal.Msg)
	}
	if store.count() != 1 {
		t.Error("a terminal recall failure must create no plan")
	}
	if after := store.stored("plan-1"); !reflect.DeepEqual(before, after) {
		t.Error("a terminal recall failure must leave the superseded plan untouched")
	}
}

// TestReingestingADateWithNoLiveRoutesIsUnchanged is the common case, and it is
// the one this gate must not tax. Re-planning a day is ordinary dispatch work;
// it acquires no approval prompt, no recall, and no write to the plan it
// supersedes.
//
// The second case is the reason the gate keys on the LEDGER and not on the
// status. A plan can read PUSHED and have nothing whatever on the board — every
// route already recalled by an earlier re-assignment — and re-planning that
// date must stay as frictionless as re-planning one that was never pushed.
func TestReingestingADateWithNoLiveRoutesIsUnchanged(t *testing.T) {
	cases := []struct {
		name string
		prev func() *Plan
	}{{
		name: "a previous plan that never reached the board",
		prev: func() *Plan {
			p := multiTruckPushReadyPlan(2)
			p.Status = StatusReviewed
			return p
		},
	}, {
		name: "a PUSHED plan whose every route has already been recalled",
		prev: func() *Plan {
			p := multiTruckPushReadyPlan(2)
			p.Status = StatusPushed
			at := timePtr()
			p.LiveRoutes = []LiveRoute{
				{VehicleID: "v1", VehicleName: "Flatbed 1", PushedAt: *at, RecalledAt: at, RecalledBy: "dispatcher@dealer.com"},
				{VehicleID: "v2", VehicleName: "Flatbed 2", PushedAt: *at, RecalledAt: at, RecalledBy: "dispatcher@dealer.com"},
			}
			return p
		},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakePlanStore(tc.prev())
			g := fleetOf("v1", "v2")
			svc := newTestService(store, g, Config{})
			before := store.stored("plan-1")
			updates := store.updates

			got, err := svc.Ingest(context.Background(), IngestRequest{Date: "2026-06-26"})
			if err != nil {
				t.Fatalf("re-planning a date with nothing on the board must just work: %v", err)
			}
			if got.Status != StatusAnalyzed {
				t.Errorf("status = %q, want %q", got.Status, StatusAnalyzed)
			}
			if len(g.recalled) != 0 {
				t.Errorf("nothing was live, so nothing may be recalled, got %v", g.recalledIDs())
			}
			// No approval, no tombstone, no write at all: the superseded plan
			// is not touched, which is what "no friction" has to mean.
			if store.updates != updates {
				t.Errorf("the superseded plan was written %d extra time(s); a date with nothing live must cost nothing",
					store.updates-updates)
			}
			if after := store.stored("plan-1"); !reflect.DeepEqual(before, after) {
				t.Errorf("the previous plan must be left alone\n before: %+v\n  after: %+v", before, after)
			}
			if n := store.count(); n != 2 {
				t.Errorf("the store holds %d plans, want 2", n)
			}
		})
	}
}

// TestIngestingADateWithNoPreviousPlanIsUnchanged is the first ingest of a day:
// the lookup finds nothing, and "nothing" is not an error.
func TestIngestingADateWithNoPreviousPlanIsUnchanged(t *testing.T) {
	store := newFakePlanStore()
	g := &fakeGable{}
	svc := newTestService(store, g, Config{})

	got, err := svc.Ingest(context.Background(), IngestRequest{Date: "2026-06-26"})
	if err != nil {
		t.Fatalf("the first ingest of a date must succeed: %v", err)
	}
	if got.Status != StatusAnalyzed || got.ID == "" {
		t.Errorf("plan = %+v, want a stored ANALYZED plan", got)
	}
	if len(g.recalled) != 0 {
		t.Errorf("nothing to supersede, so nothing to recall, got %v", g.recalledIDs())
	}
	if n := store.count(); n != 1 {
		t.Errorf("the store holds %d plans, want 1", n)
	}
}

// lookupErrStore fails the by-date lookup (the plan store is unreachable).
type lookupErrStore struct {
	*fakePlanStore
	err error
}

func (s lookupErrStore) GetLatestForDate(context.Context, string) (*Plan, error) {
	return nil, s.err
}

// TestAnUnreadableDateLookupRefusesRatherThanPlanBlind pins the fail-closed
// direction. If this module cannot find out what is already holding the date,
// it does not know whether the board is live — and creating a plan on top of an
// unknown board is precisely the state this whole change removes.
func TestAnUnreadableDateLookupRefusesRatherThanPlanBlind(t *testing.T) {
	inner := newFakePlanStore()
	store := lookupErrStore{fakePlanStore: inner, err: errors.New("connection refused")}
	svc := newTestService(store, &fakeGable{}, Config{})

	if _, err := svc.Ingest(context.Background(), IngestRequest{Date: "2026-06-26"}); err == nil {
		t.Fatal("an unreadable lookup must refuse, not plan blind")
	}
	if n := inner.count(); n != 0 {
		t.Errorf("the store holds %d plans, want 0", n)
	}
}

// TestReingestRefusalAnswers423 pins the wire contract. The UI has exactly one
// override prompt and it opens on 423; answering 502 would tell the dispatcher
// GableLBM was down, when in fact it is holding live routes somebody has to
// agree to withdraw — a dead end on screen with no way to proceed.
func TestReingestRefusalAnswers423(t *testing.T) {
	svc, _, _ := livePlanForDate(t, 2)

	mux := http.NewServeMux()
	NewHandler(svc).RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/workflow/plans",
		strings.NewReader(`{"date":"2026-06-26"}`)))

	if rec.Code != http.StatusLocked {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusLocked, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "dispatch board") || !strings.Contains(body, "manual approval") {
		t.Errorf("body %s must carry this module's own sentence, not a generic failure", body)
	}

	// And the override goes through the same endpoint with the same two fields.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/workflow/plans",
		strings.NewReader(`{"date":"2026-06-26","override":true,"approved_by":"dispatcher@dealer.com"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("an approved re-ingest must answer 201, got %d (body %s)", rec.Code, rec.Body.String())
	}
}
