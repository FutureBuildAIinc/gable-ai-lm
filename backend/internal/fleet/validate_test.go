// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package fleet

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// validProfile is a plausible three-axle flatbed. Tests mutate a copy of it so
// each case isolates exactly one defect.
func validProfile() ProfileInput {
	return ProfileInput{
		Name:          "Truck 12",
		BedLengthIn:   288,
		BedWidthIn:    96,
		BedHeightIn:   48,
		GVWRLbs:       33000,
		TareWeightLbs: 12000,
		Axles: []AxleInput{
			{AxleNumber: 1, MaxWeightLbs: 12000, PositionFromFrontIn: 0, AxleType: "STEER"},
			{AxleNumber: 2, MaxWeightLbs: 20000, PositionFromFrontIn: 180, AxleType: "DRIVE"},
			{AxleNumber: 3, MaxWeightLbs: 20000, PositionFromFrontIn: 234, AxleType: "TAG"},
		},
	}
}

func TestValidateAcceptsAPlausibleProfile(t *testing.T) {
	if err := validProfile().Validate(); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*ProfileInput)
		wantHint string
	}{
		{"blank name", func(p *ProfileInput) { p.Name = "   " }, "name"},
		{"zero bed length", func(p *ProfileInput) { p.BedLengthIn = 0 }, "bed_length_in"},
		{"negative bed width", func(p *ProfileInput) { p.BedWidthIn = -1 }, "bed_width_in"},
		{"zero bed height", func(p *ProfileInput) { p.BedHeightIn = 0 }, "bed_height_in"},
		// The regression this whole file exists for: a blank rating stored
		// through the API is read by the solver as "unrated".
		{"zero GVWR", func(p *ProfileInput) { p.GVWRLbs = 0 }, "gvwr_lbs"},
		{"negative GVWR", func(p *ProfileInput) { p.GVWRLbs = -100 }, "gvwr_lbs"},
		{"zero tare", func(p *ProfileInput) { p.TareWeightLbs = 0 }, "tare_weight_lbs"},
		{"no axles", func(p *ProfileInput) { p.Axles = nil }, "axles"},
		{"zero axle rating", func(p *ProfileInput) { p.Axles[1].MaxWeightLbs = 0 }, "max_weight_lbs"},
		{"negative position", func(p *ProfileInput) { p.Axles[1].PositionFromFrontIn = -5 }, "position_from_front_in"},
		{"unknown axle type", func(p *ProfileInput) { p.Axles[1].AxleType = "MAGIC" }, "axle_type"},
		{"no steer axle", func(p *ProfileInput) { p.Axles[0].AxleType = "DRIVE" }, "STEER"},
		{"two steer axles", func(p *ProfileInput) { p.Axles[1].AxleType = "STEER" }, "STEER"},
		{"duplicate axle number", func(p *ProfileInput) { p.Axles[2].AxleNumber = 2 }, "duplicate"},
		{"zero axle number", func(p *ProfileInput) { p.Axles[0].AxleNumber = 0 }, "axle_number"},
		// An axle ordered behind another but positioned ahead of it makes the
		// lever arms meaningless.
		{"non-monotonic positions", func(p *ProfileInput) { p.Axles[2].PositionFromFrontIn = 10 }, "must increase"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validProfile()
			tc.mutate(&p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("expected rejection, got nil")
			}
			if !errors.Is(err, ErrInvalidProfile) {
				t.Errorf("error does not match ErrInvalidProfile: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantHint) {
				t.Errorf("message %q does not mention %q", err.Error(), tc.wantHint)
			}
		})
	}
}

// An operator fixing a form should see every problem at once, not one per save.
func TestValidateReportsEveryFieldInOnePass(t *testing.T) {
	p := validProfile()
	p.Name = ""
	p.GVWRLbs = 0
	p.BedLengthIn = 0
	err := p.Validate()
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if len(verr.Fields) < 3 {
		t.Fatalf("expected at least 3 field errors, got %d: %v", len(verr.Fields), verr.Fields)
	}
}

// The service is the enforcement point, so an invalid payload must never reach
// the repository. A nil repo proves it: reaching storage would panic.
func TestServiceRejectsBeforeTouchingTheRepository(t *testing.T) {
	svc := NewService(nil)
	p := validProfile()
	p.GVWRLbs = 0
	if _, err := svc.UpsertProfile(t.Context(), "veh-1", p); !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("expected ErrInvalidProfile, got %v", err)
	}
}

// The whole point of validating server-side is that a direct PUT — bypassing
// the browser form — is refused with a 400 the operator can act on.
func TestUpsertHandlerRejectsInvalidProfileWith400(t *testing.T) {
	h := NewHandler(NewService(nil))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	p := validProfile()
	p.GVWRLbs = 0 // the blank-rating case the UI already refuses
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/fleet/profiles/veh-1", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "gvwr_lbs") {
		t.Errorf("response does not name the offending field: %s", rec.Body.String())
	}
}
