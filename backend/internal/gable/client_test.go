// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package gable

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// The ERP seam had no tests at all, which is why the two things this file pins
// were both possible: a config keystroke that broke only write-back, and a set
// of null/empty upstream answers nobody had asserted the client's behaviour on.
//
// Everything here drives the client against a real http.ServeMux with Go 1.22
// method patterns — the same router GableLBM serves with — so a redirect, a
// method mismatch or a header loss shows up the way it would in production
// rather than the way a hand-rolled stub would let it.

// integrationMux is a stand-in for GableLBM's /api/integration surface,
// registered with the same method+path patterns the real handler uses. It
// records every request that reaches a handler.
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Key    string
	Accept string
	Body   string
}

func integrationMux(t *testing.T, seen *[]recordedRequest) *http.ServeMux {
	t.Helper()
	record := func(w http.ResponseWriter, r *http.Request, status int, body string) {
		raw, _ := io.ReadAll(r.Body)
		*seen = append(*seen, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Key:    r.Header.Get("X-Integration-Key"),
			Accept: r.Header.Get("Accept"),
			Body:   string(raw),
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != "" {
			_, _ = io.WriteString(w, body)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/integration/vehicles", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, http.StatusOK, `[{"id":"v1","name":"Flatbed 1","vehicle_type":"FLATBED","capacity_weight_lbs":20000}]`)
	})
	mux.HandleFunc("GET /api/integration/locations", func(w http.ResponseWriter, r *http.Request) {
		// One geocoded yard and one that has never been geocoded — GableLBM
		// omits the coordinate keys entirely for the latter.
		record(w, r, http.StatusOK, `[{"id":"loc1","name":"Kelowna Yard","address":"2450 Enterprise Way, Kelowna, BC V1X 7K2","latitude":49.8879,"longitude":-119.496},{"id":"loc2","name":"Vernon Yard","address":"115 Kalamalka Rd, Vernon, BC V1T 6V1"}]`)
	})
	mux.HandleFunc("GET /api/integration/drivers", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, http.StatusOK, `[{"id":"d1","name":"Sam","status":"ACTIVE"}]`)
	})
	mux.HandleFunc("GET /api/integration/products", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, http.StatusOK, `[{"id":"p1","sku":"2x4","name":"2x4x8","weight_lbs":9,"length_in":null,"width_in":null,"height_in":null,"stackable":null}]`)
	})
	mux.HandleFunc("GET /api/integration/orders", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, http.StatusOK, `[{"id":"o1","status":"CONFIRMED","branch_id":"loc1","lines":[]}]`)
	})
	mux.HandleFunc("POST /api/integration/delivery-routes", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, http.StatusCreated, "")
	})
	// GET and POST share this path upstream, which is exactly why the read is
	// registered here as its own method pattern: a client that sent the read as
	// a POST, or the push as a GET, would be answered by the other handler
	// rather than by a 405 anybody would notice.
	//
	// The body is the full range the endpoint documents: a live route, a route
	// with no truck (nullable vehicle_id), a departed one, and a CANCELLED row
	// carrying recalled_at — the row that answers "did my recall land?" and
	// that a naive status filter would hide.
	mux.HandleFunc("GET /api/integration/delivery-routes", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, http.StatusOK, `[`+
			`{"route_id":"r1","vehicle_id":"v1","driver_id":"d1","status":"SCHEDULED","scheduled_date":"2026-06-26","stop_count":2,"order_ids":["o1","o2"]},`+
			`{"route_id":"r2","vehicle_id":"","status":"DRAFT","scheduled_date":"2026-06-26","stop_count":0,"order_ids":[]},`+
			`{"route_id":"r3","vehicle_id":"v3","status":"IN_TRANSIT","scheduled_date":"2026-06-26","stop_count":1,"order_ids":["o3"]},`+
			`{"route_id":"r4","vehicle_id":"v4","status":"CANCELLED","scheduled_date":"2026-06-26","stop_count":0,"order_ids":[],"recalled_at":"2026-06-25T17:04:05Z"}`+
			`]`)
	})
	mux.HandleFunc("POST /api/integration/validate-staff", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, http.StatusOK, `{"staff_id":"s1","email":"a@b.c","name":"A","entitled":true,"roles":["DISPATCH"],"modules":["AI_LM"]}`)
	})
	return mux
}

// TestBaseURLTrailingSlashDoesNotBreakWriteBack is the regression guard for a
// one-keystroke deployment defect.
//
// GABLE_API_URL is typed by a human into an app spec or a .env. Given
// "http://host:8080/" every path became "//api/integration/…". http.ServeMux
// cleans that and answers 301; Go's http.Client follows the redirect but
// downgrades the POST to a GET and drops the body. The result was the worst
// available shape of failure: every READ still worked, so the Load Builder,
// the catalog and the whole guided workflow looked healthy — and only
// PushDeliveryRoute, the single call that puts a route on the dispatch board,
// failed, reporting a status for a POST the server was never sent.
func TestBaseURLTrailingSlashDoesNotBreakWriteBack(t *testing.T) {
	for _, suffix := range []string{"", "/", "///"} {
		t.Run("baseURL suffix "+suffixLabel(suffix), func(t *testing.T) {
			var seen []recordedRequest
			srv := httptest.NewServer(integrationMux(t, &seen))
			defer srv.Close()

			c := NewClient(srv.URL+suffix, "test-key")

			if _, err := c.ListVehicles(context.Background()); err != nil {
				t.Fatalf("ListVehicles: %v", err)
			}
			route := DeliveryRoute{
				VehicleID:     "v1",
				DriverID:      "d1",
				ScheduledDate: "2026-06-26",
				Stops:         []RouteStop{{OrderID: "o1", Sequence: 1, Lat: 49.9, Lng: -119.5}},
				LoadManifest:  map[string]any{"version": 2},
			}
			if err := c.PushDeliveryRoute(context.Background(), route); err != nil {
				t.Fatalf("PushDeliveryRoute: %v", err)
			}

			if len(seen) != 2 {
				t.Fatalf("expected 2 requests to reach a handler, got %d: %+v", len(seen), seen)
			}
			push := seen[1]
			if push.Method != http.MethodPost {
				t.Errorf("write-back reached GableLBM as %s, not POST", push.Method)
			}
			if push.Path != "/api/integration/delivery-routes" {
				t.Errorf("write-back path was %q", push.Path)
			}
			if push.Key != "test-key" {
				t.Errorf("X-Integration-Key was %q — a redirect can strip it", push.Key)
			}
			// The body is the whole point: a redirected POST arrives empty.
			var decoded DeliveryRoute
			if err := json.Unmarshal([]byte(push.Body), &decoded); err != nil {
				t.Fatalf("write-back body was not the route (%q): %v", push.Body, err)
			}
			if decoded.VehicleID != "v1" || len(decoded.Stops) != 1 {
				t.Errorf("write-back body lost content: %+v", decoded)
			}
		})
	}
}

// suffixLabel renders the suffix readably in a subtest name.
func suffixLabel(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// TestReadsCarryTheIntegrationKeyAndQuery pins the request shape every read
// depends on: the key header, the JSON Accept, and the date+status filter the
// order pull is defined by.
func TestReadsCarryTheIntegrationKeyAndQuery(t *testing.T) {
	var seen []recordedRequest
	srv := httptest.NewServer(integrationMux(t, &seen))
	defer srv.Close()
	c := NewClient(srv.URL, "test-key")

	if _, err := c.ListOrdersForDate(context.Background(), "2026-06-26"); err != nil {
		t.Fatalf("ListOrdersForDate: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("expected 1 request, got %d", len(seen))
	}
	got := seen[0]
	if got.Key != "test-key" {
		t.Errorf("X-Integration-Key = %q", got.Key)
	}
	if got.Accept != "application/json" {
		t.Errorf("Accept = %q", got.Accept)
	}
	if !strings.Contains(got.Query, "date=2026-06-26") || !strings.Contains(got.Query, "status=CONFIRMED") {
		t.Errorf("order pull query = %q, want the date and CONFIRMED filter", got.Query)
	}
}

// TestNullableProductGeometrySurvivesTheWire pins the distinction the whole
// catalog fallback rests on: GableLBM sends null L/W/H for a product the PIM
// has never measured, and null must NOT arrive here as a real zero dimension.
func TestNullableProductGeometrySurvivesTheWire(t *testing.T) {
	var seen []recordedRequest
	srv := httptest.NewServer(integrationMux(t, &seen))
	defer srv.Close()

	products, err := NewClient(srv.URL, "k").GetProductsWithWeight(context.Background())
	if err != nil {
		t.Fatalf("GetProductsWithWeight: %v", err)
	}
	if len(products) != 1 {
		t.Fatalf("expected 1 product, got %d", len(products))
	}
	p := products[0]
	if p.LengthIn != nil || p.WidthIn != nil || p.HeightIn != nil {
		t.Errorf("null geometry decoded as a value (%v/%v/%v) — an unmeasured SKU would be packed as a zero-size box",
			p.LengthIn, p.WidthIn, p.HeightIn)
	}
	if p.Stackable != nil {
		t.Error("null stackable decoded as a value — the catalog default must decide, not the wire")
	}
	if p.WeightLbs != 9 {
		t.Errorf("weight = %v, want 9", p.WeightLbs)
	}
}

// TestNullableBranchGeometrySurvivesTheWire is the Location twin of the product
// test above, and it guards the decision Phase 1 depot resolution rests on: a
// branch GableLBM has never geocoded arrives with its coordinate keys ABSENT,
// and absent must decode to nil — not to 0,0. Rooting a dealer's whole day at
// (0,0) would be worse than having no depot at all, because it looks like a
// real answer.
func TestNullableBranchGeometrySurvivesTheWire(t *testing.T) {
	var seen []recordedRequest
	srv := httptest.NewServer(integrationMux(t, &seen))
	defer srv.Close()

	locs, err := NewClient(srv.URL, "k").ListLocations(context.Background())
	if err != nil {
		t.Fatalf("ListLocations: %v", err)
	}
	if len(locs) != 2 {
		t.Fatalf("expected 2 locations, got %d", len(locs))
	}
	if locs[0].Latitude == nil || locs[0].Longitude == nil {
		t.Fatalf("geocoded yard lost its coordinates: %+v", locs[0])
	}
	if *locs[0].Latitude != 49.8879 || *locs[0].Longitude != -119.496 {
		t.Errorf("geocoded yard = (%v, %v), want (49.8879, -119.496)", *locs[0].Latitude, *locs[0].Longitude)
	}
	if locs[1].Latitude != nil || locs[1].Longitude != nil {
		t.Errorf("an ungeocoded yard decoded to a coordinate (%v, %v) — a route would be rooted at a place that was never resolved",
			locs[1].Latitude, locs[1].Longitude)
	}
	if locs[1].Name != "Vernon Yard" || locs[1].Address == "" {
		t.Errorf("ungeocoded yard lost its identity: %+v", locs[1])
	}

	got := seen[len(seen)-1]
	if got.Method != http.MethodGet || got.Path != "/api/integration/locations" {
		t.Errorf("called %s %s, want GET /api/integration/locations", got.Method, got.Path)
	}
	if got.Key != "k" {
		t.Errorf("X-Integration-Key = %q, want the configured key", got.Key)
	}
}

// TestOrderCarriesItsBranch pins that the yard an order ships from survives the
// wire; without it the branch depot silently degrades to the old global one.
func TestOrderCarriesItsBranch(t *testing.T) {
	var seen []recordedRequest
	srv := httptest.NewServer(integrationMux(t, &seen))
	defer srv.Close()

	orders, err := NewClient(srv.URL, "k").ListOrdersForDate(context.Background(), "2026-06-26")
	if err != nil {
		t.Fatalf("ListOrdersForDate: %v", err)
	}
	if len(orders) != 1 || orders[0].BranchID != "loc1" {
		t.Fatalf("order branch = %+v, want branch_id loc1", orders)
	}
}

// TestUpstreamAnswersThatAreNotArrays pins what the client does with the three
// shapes a real ERP produces on a bad day. None of them may be silently read as
// a successful empty answer, and none may panic.
func TestUpstreamAnswersThatAreNotArrays(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr bool
		// wantEmpty asserts a successful call returned no rows.
		wantEmpty bool
	}{
		{
			// The exact defect class found in `gable`: an empty result
			// serialised as JSON null rather than []. It decodes to a nil
			// slice, and every consumer here ranges over it, so it is an
			// EMPTY fleet — not an error and not a crash.
			name: "bare null instead of an empty array", status: 200, body: `null`, wantEmpty: true,
		},
		{name: "empty array", status: 200, body: `[]`, wantEmpty: true},
		{
			// A 200 with no body at all (a misconfigured proxy). This must be
			// an error, not an empty fleet: "no trucks today" and "we could not
			// ask" are different answers and only one of them is safe to act on.
			name: "200 with an empty body", status: 200, body: ``, wantErr: true,
		},
		{
			// An enveloped object where the contract says bare array. Silently
			// reading it as an empty fleet would strand every order.
			name: "enveloped object instead of a bare array", status: 200, body: `{"data":[]}`, wantErr: true,
		},
		{name: "upstream 500 with an HTML error page", status: 500, body: `<html>boom</html>`, wantErr: true},
		{name: "upstream 401 on a bad integration key", status: 401, body: `{"error":"unauthorized"}`, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				if tc.body != "" {
					_, _ = io.WriteString(w, tc.body)
				}
			}))
			defer srv.Close()

			vehicles, err := NewClient(srv.URL, "k").ListVehicles(context.Background())
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("expected an error, got %d vehicles", len(vehicles))
			case !tc.wantErr && err != nil:
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantEmpty && len(vehicles) != 0 {
				t.Fatalf("expected no vehicles, got %d", len(vehicles))
			}
			if err != nil && !strings.Contains(err.Error(), "/api/integration/vehicles") {
				t.Errorf("the error must name the call that failed, got %q", err.Error())
			}
		})
	}
}

// TestErrorCarriesUpstreamStatusAndSnippet pins that an upstream refusal is
// reported with GableLBM's own status and message rather than flattened into a
// generic failure — that is how an operator tells "our key is wrong" from
// "their database is down".
func TestErrorCarriesUpstreamStatusAndSnippet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"invalid integration key"}`)
	}))
	defer srv.Close()

	err := NewClient(srv.URL, "wrong").PushDeliveryRoute(context.Background(), DeliveryRoute{VehicleID: "v1"})
	if err == nil {
		t.Fatal("a 403 must not read as a successful push")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "invalid integration key") {
		t.Errorf("error %q should carry the upstream status and its message", err.Error())
	}
}

// TestValidateStaffNullBodyIsNotAnEntitlement pins the fail-closed direction of
// the auth seam: a 200 carrying `null` decodes to the zero value, and the zero
// value must be "not entitled".
func TestValidateStaffNullBodyIsNotAnEntitlement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `null`)
	}))
	defer srv.Close()

	sv, err := NewClient(srv.URL, "k").ValidateStaff(context.Background(), "a@b.c")
	if err != nil {
		t.Fatalf("ValidateStaff: %v", err)
	}
	if sv == nil {
		t.Fatal("expected a zero-valued validation, not nil")
	}
	if sv.Entitled {
		t.Error("a null validation body must never read as entitled")
	}
}

// TestContextCancellationIsReportedAsAFailure pins that a cancelled workflow
// does not read as an empty fleet.
func TestContextCancellationIsReportedAsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewClient(srv.URL, "k").ListVehicles(ctx); err == nil {
		t.Fatal("a cancelled context must surface as an error, not an empty fleet")
	}
}

// TestRecallDeliveryRouteSendsTheContractedShape pins the request AI_LM makes
// against GableLBM's recall endpoint: the path, the integration key, and the
// (vehicle_id, scheduled_date) key plus its optional provenance. The pair is
// the key on purpose — see RouteRecall on why a stored route id is not.
func TestRecallDeliveryRouteSendsTheContractedShape(t *testing.T) {
	var gotPath, gotMethod, gotKey string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotKey = r.URL.Path, r.Method, r.Header.Get("X-Integration-Key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"recalled":true,"route_id":"r-9","stop_count":3,"reason":"dropped by re-assignment"}`)
	}))
	defer srv.Close()

	res, err := NewClient(srv.URL, "k").RecallDeliveryRoute(context.Background(), RouteRecall{
		VehicleID:     "v1",
		ScheduledDate: "2026-06-26",
		Reason:        "dropped by re-assignment",
		RecalledBy:    "dispatcher@dealer.com",
	})
	if err != nil {
		t.Fatalf("RecallDeliveryRoute: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/integration/delivery-routes/recall" {
		t.Errorf("called %s %s, want POST /api/integration/delivery-routes/recall", gotMethod, gotPath)
	}
	if gotKey != "k" {
		t.Errorf("integration key = %q, want %q", gotKey, "k")
	}
	for field, want := range map[string]any{
		"vehicle_id":     "v1",
		"scheduled_date": "2026-06-26",
		"reason":         "dropped by re-assignment",
		"recalled_by":    "dispatcher@dealer.com",
	} {
		if gotBody[field] != want {
			t.Errorf("body[%q] = %v, want %v", field, gotBody[field], want)
		}
	}
	if !res.Recalled || res.RouteID != "r-9" || res.StopCount != 3 {
		t.Errorf("result = %+v, want the upstream acknowledgement decoded", res)
	}
}

// TestRecallingNothingIsSuccess pins the convergence contract. Recall exists to
// clean up after a partial push, so it is retried; if "there was nothing on the
// board" read as a failure, the caller could never tell converged from broken
// and would refuse to advance a plan that was already correct.
func TestRecallingNothingIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"recalled":false}`)
	}))
	defer srv.Close()

	res, err := NewClient(srv.URL, "k").RecallDeliveryRoute(context.Background(),
		RouteRecall{VehicleID: "v1", ScheduledDate: "2026-06-26"})
	if err != nil {
		t.Fatalf("an empty board must not read as a failed recall: %v", err)
	}
	if res.Recalled {
		t.Error("recalled = true, want false when there was nothing to withdraw")
	}
}

// TestRecallOfADispatchedRouteIsTerminal pins that a truck already on the road
// is reported as its own sentinel. Retrying it forever would be wrong: the
// answer is a phone call, not a loop.
func TestRecallOfADispatchedRouteIsTerminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"route already dispatched; cannot recall"}`)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "k").RecallDeliveryRoute(context.Background(),
		RouteRecall{VehicleID: "v1", ScheduledDate: "2026-06-26"})
	if !errors.Is(err, ErrRouteDispatched) {
		t.Fatalf("a 409 must surface as ErrRouteDispatched, got %v", err)
	}
	if !strings.Contains(err.Error(), "v1") {
		t.Errorf("error %q should name the truck that cannot be recalled", err)
	}
}

// TestRecallFailureIsNotSilentlySuccessful pins the fail-closed direction: a
// 500 upstream must not read as "the board is clear". The workflow refuses to
// rewrite a plan when a recall fails, so this error is what stops a truck's
// route being abandoned live on the dealer's board.
func TestRecallFailureIsNotSilentlySuccessful(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"failed to recall delivery route"}`)
	}))
	defer srv.Close()

	res, err := NewClient(srv.URL, "k").RecallDeliveryRoute(context.Background(),
		RouteRecall{VehicleID: "v1", ScheduledDate: "2026-06-26"})
	if err == nil {
		t.Fatal("a 500 must not read as a successful recall")
	}
	if res != nil {
		t.Error("a failed recall must not return a result a caller could act on")
	}
	if errors.Is(err, ErrRouteDispatched) {
		t.Error("a 500 is retryable and must not be reported as the terminal 409 case")
	}
}

// TestAPIErrorCarriesTheStatusCodeMachinesNeed pins that the upstream status is
// reachable programmatically, not only inside the message. RecallDeliveryRoute's
// 409 branch depends on it, and before APIError existed there was no way to
// write that branch except by matching English.
func TestAPIErrorCarriesTheStatusCodeMachinesNeed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"nope"}`)
	}))
	defer srv.Close()

	err := NewClient(srv.URL, "k").PushDeliveryRoute(context.Background(), DeliveryRoute{VehicleID: "v1"})
	var target *APIError
	if !errors.As(err, &target) {
		t.Fatalf("upstream failure %v should be an *APIError", err)
	}
	if target.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want 404", target.Status)
	}
	if target.Method != http.MethodPost || target.Path != "/api/integration/delivery-routes" {
		t.Errorf("APIError should name the call: %+v", target)
	}
}

// TestReadingTheDispatchBoardIsTheSeamsFirstREAD pins the call that lets AI_LM
// stop trusting its own ledger.
//
// Every other GET on this seam is planning INPUT — vehicles, orders, products,
// branches. This one is the system of record: what the dealer's board actually
// holds. Four properties are asserted because dropping any one of them puts the
// caller back to guessing.
func TestReadingTheDispatchBoardIsTheSeamsFirstREAD(t *testing.T) {
	var seen []recordedRequest
	srv := httptest.NewServer(integrationMux(t, &seen))
	defer srv.Close()
	c := NewClient(srv.URL, "test-key")

	routes, err := c.ListDeliveryRoutesForDate(context.Background(), "2026-06-26")
	if err != nil {
		t.Fatalf("ListDeliveryRoutesForDate: %v", err)
	}

	// 1. It is a GET on the path the push POSTs to, carrying the key and the
	//    required date. Sending it as a POST would reach ReplaceDeliveryRoute.
	if len(seen) != 1 {
		t.Fatalf("expected 1 request, got %d", len(seen))
	}
	got := seen[0]
	if got.Method != http.MethodGet || got.Path != "/api/integration/delivery-routes" {
		t.Errorf("board read went out as %s %s", got.Method, got.Path)
	}
	if got.Key != "test-key" || got.Accept != "application/json" {
		t.Errorf("board read lost its headers: %+v", got)
	}
	if got.Query != "date=2026-06-26" {
		t.Errorf("board read query = %q; date is REQUIRED upstream and a dateless call is a 400, not an unfiltered dump", got.Query)
	}

	// 2. It decodes as a bare array, in order, with every field.
	if len(routes) != 4 {
		t.Fatalf("decoded %d routes, want 4: %+v", len(routes), routes)
	}
	if r := routes[0]; r.RouteID != "r1" || r.VehicleID != "v1" || r.DriverID != "d1" ||
		r.Status != RouteStatusScheduled || r.ScheduledDate != "2026-06-26" ||
		r.StopCount != 2 || len(r.OrderIDs) != 2 || r.OrderIDs[0] != "o1" {
		t.Errorf("live route lost detail: %+v", r)
	}

	// 3. The LIVE/DISPATCHED split, which is the product decision this type
	//    exists to carry. Getting it wrong in one direction proposes cancelling
	//    a run that is physically happening; in the other it leaves a route on
	//    the board that a re-plan then silently plans over.
	for i, want := range []struct{ live, dispatched bool }{
		{live: true},       // SCHEDULED
		{live: true},       // DRAFT, no vehicle
		{dispatched: true}, // IN_TRANSIT
		{},                 // CANCELLED — neither
	} {
		if routes[i].Live() != want.live || routes[i].Dispatched() != want.dispatched {
			t.Errorf("route %d (%s): Live()=%v Dispatched()=%v, want %v/%v",
				i, routes[i].Status, routes[i].Live(), routes[i].Dispatched(), want.live, want.dispatched)
		}
	}

	// 4. A CANCELLED row is RETURNED, with recalled_at. Hiding it would make a
	//    landed recall indistinguishable from a route that never existed —
	//    which is the exact blind spot this endpoint removes.
	if r := routes[3]; r.Status != RouteStatusCancelled || r.RecalledAt != "2026-06-25T17:04:05Z" {
		t.Errorf("the cancelled row must survive with its recall stamp: %+v", r)
	}
	// A route with no truck is not addressable by the (vehicle_id, date) recall
	// key, and the empty string is how that arrives.
	if routes[1].VehicleID != "" {
		t.Errorf("a nullable vehicle_id must arrive as empty, got %q", routes[1].VehicleID)
	}
}

// TestAnEmptyBoardIsNotAFailure covers the two shapes an empty date can take.
// A caller reconciling against the board must be able to read "nothing is on
// this day" as a fact, and a nil slice and an empty one must mean the same
// thing here — the difference between them is exactly the difference between
// "the board is clear" and "something went wrong", and only the error says the
// second.
func TestAnEmptyBoardIsNotAFailure(t *testing.T) {
	for _, body := range []string{`[]`, `null`} {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/integration/delivery-routes", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		})
		srv := httptest.NewServer(mux)
		c := NewClient(srv.URL, "k")
		routes, err := c.ListDeliveryRoutesForDate(context.Background(), "2026-06-26")
		if err != nil {
			t.Errorf("body %s: an empty board is not an error: %v", body, err)
		}
		if len(routes) != 0 {
			t.Errorf("body %s: decoded %d routes", body, len(routes))
		}
		srv.Close()
	}
}

// TestAnUnreadableBoardIsAnError is the other half, and it is the one the
// caller's fail-closed rule rests on. A 500 from GableLBM must NOT decode to an
// empty board: "the dealer has nothing scheduled" and "we could not find out"
// are opposite answers, and only one of them makes it safe to re-plan the day.
func TestAnUnreadableBoardIsAnError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/integration/delivery-routes", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"list delivery routes"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	routes, err := NewClient(srv.URL, "k").ListDeliveryRoutesForDate(context.Background(), "2026-06-26")
	if err == nil {
		t.Fatalf("a 500 decoded to %d routes with no error — a failed read must never read as an empty board", len(routes))
	}
	if routes != nil {
		t.Errorf("a failed read returned %v alongside its error", routes)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Errorf("the caller needs the status to act on: %v", err)
	}
}

// TestBoardRouteStillMirrorsTheERPContract is this repo's half of a cross-repo
// seam, and it exists because the OTHER half cannot see this file.
//
// GableLBM pins the mirror in internal/integrations/ailm_contract_test.go by
// keeping a HAND-COPIED aiLMBoardRoute beside a golden
// (internal/integrations/testdata/ailm_client_contract.json, type
// "BoardRoute"). That suite reduces its own copy and compares it to the golden
// — so it catches somebody editing GableLBM, and is structurally blind to
// somebody editing the type below. Nothing in either repo's CI opens both
// checkouts at once. Without this test, a field renamed here builds clean,
// tests clean, ships, and then decodes as a zero value against a perfectly
// healthy ERP.
//
// So the reduction is repeated here, deliberately, in the SAME shape GableLBM
// reduces to: json name, Go type, pointer, omitempty, IN ORDER. A change to
// BoardRoute must break this test, and the fix is never to edit the expectation
// alone — it is to re-sync GableLBM's copy and regenerate its golden with
// -update-golden.
//
// Only BoardRoute and RouteRecall are pinned: they are the two types whose
// shape a caller cannot recover from if it is wrong. A recall keyed on the
// wrong field silently withdraws nothing (or the wrong truck), and a board row
// that decodes without its status makes every route on the board look
// reclaimable.
func TestBoardRouteStillMirrorsTheERPContract(t *testing.T) {
	type field struct {
		name      string
		goType    string
		pointer   bool
		omitEmpty bool
	}
	for _, tc := range []struct {
		typ  any
		want []field
	}{{
		typ: BoardRoute{},
		want: []field{
			{name: "route_id", goType: "string"},
			{name: "vehicle_id", goType: "string"},
			{name: "driver_id", goType: "string", omitEmpty: true},
			{name: "status", goType: "string"},
			{name: "scheduled_date", goType: "string"},
			{name: "stop_count", goType: "int"},
			{name: "order_ids", goType: "[]string"},
			{name: "recalled_at", goType: "string", omitEmpty: true},
		},
	}, {
		typ: RouteRecall{},
		want: []field{
			{name: "vehicle_id", goType: "string"},
			{name: "scheduled_date", goType: "string"},
			{name: "reason", goType: "string", omitEmpty: true},
			{name: "recalled_by", goType: "string", omitEmpty: true},
		},
	}} {
		rt := reflect.TypeOf(tc.typ)
		t.Run(rt.Name(), func(t *testing.T) {
			if rt.NumField() != len(tc.want) {
				t.Fatalf("%s has %d fields, the ERP mirror pins %d — re-sync internal/integrations/ailm_contract_test.go and regenerate its golden",
					rt.Name(), rt.NumField(), len(tc.want))
			}
			for i, want := range tc.want {
				f := rt.Field(i)
				parts := strings.Split(f.Tag.Get("json"), ",")
				name, omit := parts[0], false
				for _, p := range parts[1:] {
					if p == "omitempty" {
						omit = true
					}
				}
				ft := f.Type
				ptr := ft.Kind() == reflect.Pointer
				if ptr {
					ft = ft.Elem()
				}
				if name != want.name || ft.String() != want.goType || ptr != want.pointer || omit != want.omitEmpty {
					t.Errorf("%s field %d is {%s %s ptr=%v omitempty=%v}, the ERP mirror pins {%s %s ptr=%v omitempty=%v}",
						rt.Name(), i, name, ft.String(), ptr, omit, want.name, want.goType, want.pointer, want.omitEmpty)
				}
			}
		})
	}
}

// TestStatusConstantsAreTheERPsOwnValues pins the strings, not just the shape.
// Status is only useful because both sides spell delivery_routes.status the
// same way; a typo here would classify every route as neither live nor
// dispatched, which fails OPEN — a route on the board that looks like nothing
// at all is a route a re-plan walks straight over.
func TestStatusConstantsAreTheERPsOwnValues(t *testing.T) {
	for got, want := range map[string]string{
		RouteStatusDraft:     "DRAFT",
		RouteStatusScheduled: "SCHEDULED",
		RouteStatusInTransit: "IN_TRANSIT",
		RouteStatusCompleted: "COMPLETED",
		RouteStatusCancelled: "CANCELLED",
	} {
		if got != want {
			t.Errorf("status constant = %q, want %q (GableLBM delivery_routes.status)", got, want)
		}
	}
	// Every status the ERP can hold must fall into exactly one bucket, or a
	// route classified as neither is a route no gate will ever see.
	for _, st := range []string{RouteStatusDraft, RouteStatusScheduled, RouteStatusInTransit, RouteStatusCompleted, RouteStatusCancelled} {
		r := BoardRoute{Status: st}
		if r.Live() && r.Dispatched() {
			t.Errorf("%s is both live and dispatched", st)
		}
		if st != RouteStatusCancelled && !r.Live() && !r.Dispatched() {
			t.Errorf("%s is neither live nor dispatched — it would be invisible to every gate", st)
		}
	}
}
