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
