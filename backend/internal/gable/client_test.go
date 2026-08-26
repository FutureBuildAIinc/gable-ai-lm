// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package gable

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	mux.HandleFunc("GET /api/integration/drivers", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, http.StatusOK, `[{"id":"d1","name":"Sam","status":"ACTIVE"}]`)
	})
	mux.HandleFunc("GET /api/integration/products", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, http.StatusOK, `[{"id":"p1","sku":"2x4","name":"2x4x8","weight_lbs":9,"length_in":null,"width_in":null,"height_in":null,"stackable":null}]`)
	})
	mux.HandleFunc("GET /api/integration/orders", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, http.StatusOK, `[{"id":"o1","status":"CONFIRMED","lines":[]}]`)
	})
	mux.HandleFunc("POST /api/integration/delivery-routes", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, http.StatusCreated, "")
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
