// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
)

// failingGable stands in for a GableLBM that is genuinely unreachable, so the
// 502 side of the mapping is exercised by a real upstream fault rather than by
// a validation error wearing an upstream label.
type failingGable struct{ *fakeGable }

func (failingGable) ListOrdersForDate(context.Context, string) ([]gable.Order, error) {
	return nil, errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")
}

// TestIngestSeparatesClientErrorsFromUpstreamFailures is the regression guard
// for the ingest status mapping. POSTing an empty body answered
// 502 UPSTREAM_ERROR — "GableLBM is down" — for what is purely a caller
// mistake, sending whoever reads that status to go check a healthy ERP.
//
// 400 for a bad request, 502 only when the upstream really did fail.
func TestIngestSeparatesClientErrorsFromUpstreamFailures(t *testing.T) {
	route := func(svc *Service) http.Handler {
		mux := http.NewServeMux()
		NewHandler(svc).RegisterRoutes(mux)
		return mux
	}
	healthy := newTestService(newFakePlanStore(), nil, Config{})
	broken := NewService(newFakePlanStore(), failingGable{&fakeGable{}}, &fakeCatalog{}, &fakeFleet{},
		&fakeChecker{}, fakeBriefer{}, Config{})

	cases := []struct {
		name     string
		svc      *Service
		body     string
		want     int
		wantCode string
	}{
		{"empty body", healthy, ``, http.StatusBadRequest, "BAD_REQUEST"},
		{"empty JSON object", healthy, `{}`, http.StatusBadRequest, "BAD_REQUEST"},
		{"blank date", healthy, `{"date":""}`, http.StatusBadRequest, "BAD_REQUEST"},
		{"malformed date", healthy, `{"date":"26-08-2026"}`, http.StatusBadRequest, "BAD_REQUEST"},
		{"not JSON at all", healthy, `date=today`, http.StatusBadRequest, "BAD_REQUEST"},
		// The genuine 502: a well-formed request the upstream could not serve.
		{"GableLBM unreachable", broken, `{"date":"2026-08-21"}`, http.StatusBadGateway, "UPSTREAM_ERROR"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/workflow/plans", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			route(tc.svc).ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("got HTTP %d, want %d (body: %s)", rec.Code, tc.want, rec.Body.String())
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("error envelope did not decode: %v (%s)", err, rec.Body.String())
			}
			if body.Error.Code != tc.wantCode {
				t.Fatalf("error code = %q, want %q", body.Error.Code, tc.wantCode)
			}
		})
	}
}

// TestRefusalsReachTheDispatcher is the regression guard for a silent module.
//
// Every gate in this package is a sentence written for the person it stops, and
// the app renders `error.message` verbatim (`aiLmService.jsonOrThrow`). None of
// them arrived: the handler collapsed each one into "workflow step failed" and
// `httputil.RespondError` then replaced even that with the HTTP status phrase.
// A dispatcher refused permission to send a truck out of the yard was told
// "Unprocessable Entity" and had to guess which of the four gates had closed.
func TestRefusalsReachTheDispatcher(t *testing.T) {
	route := func(svc *Service) http.Handler {
		mux := http.NewServeMux()
		NewHandler(svc).RegisterRoutes(mux)
		return mux
	}

	cases := []struct {
		name   string
		plan   func() *Plan
		method string
		path   string
		body   string
		want   string
	}{
		{
			name: "the yard has not signed the load off",
			plan: func() *Plan {
				p := planWithPackedLoad()
				p.Loads[0].Proof.SignedOff = false
				return p
			},
			method: http.MethodPost, path: "/api/v1/workflow/plans/plan-1/push",
			want: "yard proof + sign-off required before depart on: Flatbed 1",
		},
		{
			name: "the truck is over its own GVW rating",
			plan: func() *Plan {
				p := pushReadyPlan()
				p.Loads[0].LoadPlan.GVWStatus = "FAIL"
				return p
			},
			method: http.MethodPost, path: "/api/v1/workflow/plans/plan-1/push",
			want: "GVW FAIL",
		},
		{
			name: "cargo did not fit and would ship the customer short",
			plan: func() *Plan {
				p := pushReadyPlan()
				p.Loads[0].LoadPlan.Unplaced = droppedCargo("STONE-STEP-72")
				return p
			},
			method: http.MethodPost, path: "/api/v1/workflow/plans/plan-1/push",
			want: "did not fit and were dropped",
		},
		{
			name: "the route review has not run",
			plan: func() *Plan {
				p := pushReadyPlan()
				p.Loads[0].Compliance = nil
				return p
			},
			method: http.MethodPost, path: "/api/v1/workflow/plans/plan-1/push",
			want: "has not passed route review yet",
		},
		{
			name:   "sign-off attempted with no proof on the load",
			plan:   func() *Plan { p := planWithPackedLoad(); p.Loads[0].Proof = nil; return p },
			method: http.MethodPost, path: "/api/v1/workflow/plans/plan-1/loads/v1/sign-off",
			body: `{"signed_by":"Yard Lead"}`,
			want: "needs at least one proof photo/video before sign-off",
		},
		{
			name:   "a resequence naming a truck this plan does not have",
			plan:   pushReadyPlan,
			method: http.MethodPut, path: "/api/v1/workflow/plans/plan-1/loads/nope/sequence",
			body: `{"order_ids":["o1"]}`,
			want: "no load for vehicle nope in this plan",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakePlanStore(tc.plan())
			svc := newTestService(store, &fakeGable{}, Config{})

			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			route(svc).ServeHTTP(rec, req)

			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("got HTTP %d, want 422 (body: %s)", rec.Code, rec.Body.String())
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("error envelope did not decode: %v (%s)", err, rec.Body.String())
			}
			if !strings.Contains(body.Error.Message, tc.want) {
				t.Fatalf("the refusal the dispatcher sees is %q; it must say %q",
					body.Error.Message, tc.want)
			}
		})
	}
}

// TestUpstreamFailuresStayGenericOnTheWire is the other half of that contract.
// A refusal is this module's own sentence and is shown; a wrapped upstream
// error carries a URL and up to 512 bytes of GableLBM's response body, and is
// not.
func TestUpstreamFailuresStayGenericOnTheWire(t *testing.T) {
	mux := http.NewServeMux()
	broken := NewService(newFakePlanStore(planWithPackedLoad()), failingVehicles{&fakeGable{}},
		&fakeCatalog{}, &fakeFleet{}, &fakeChecker{}, fakeBriefer{}, Config{})
	NewHandler(broken).RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflow/plans/plan-1/pack", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got HTTP %d, want 422 (body: %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "SQLSTATE") ||
		strings.Contains(rec.Body.String(), "10.0.3.7") {
		t.Fatalf("an upstream diagnostic leaked to the client: %s", rec.Body.String())
	}
}

// failingVehicles is a GableLBM whose fleet read fails with a diagnostic no
// client may see.
type failingVehicles struct{ *fakeGable }

func (failingVehicles) ListVehicles(context.Context) ([]gable.Vehicle, error) {
	return nil, errors.New(`gable GET /api/integration/vehicles: status 500: {"db":"SQLSTATE 42703","host":"10.0.3.7"}`)
}

// TestIngestStillSucceedsOnAValidDate is the positive control: the mapping above
// did not simply start rejecting everything.
func TestIngestStillSucceedsOnAValidDate(t *testing.T) {
	mux := http.NewServeMux()
	NewHandler(newTestService(newFakePlanStore(), nil, Config{})).RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflow/plans", strings.NewReader(`{"date":"2026-08-21"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("got HTTP %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
}
