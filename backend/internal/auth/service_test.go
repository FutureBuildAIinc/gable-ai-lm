// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FutureBuildAIinc/gable-ai-lm/internal/gable"
	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/middleware"
)

type fakeValidator struct {
	out *gable.StaffValidation
	err error
}

func (f fakeValidator) ValidateStaff(_ context.Context, _ string) (*gable.StaffValidation, error) {
	return f.out, f.err
}

func TestLogin_EmailRequired(t *testing.T) {
	svc := NewService(fakeValidator{}, "secret")
	if _, err := svc.Login(context.Background(), "  "); !errors.Is(err, ErrEmailRequired) {
		t.Fatalf("expected ErrEmailRequired, got %v", err)
	}
}

func TestLogin_NotEntitled(t *testing.T) {
	svc := NewService(fakeValidator{out: &gable.StaffValidation{Entitled: false}}, "secret")
	if _, err := svc.Login(context.Background(), "nope@futurebuild.ai"); !errors.Is(err, ErrNotEntitled) {
		t.Fatalf("expected ErrNotEntitled, got %v", err)
	}
}

// TestLogin_RoundTrip mints a session token and verifies the auth middleware
// accepts it (session-secret only, no JWKS) and populates the same claims.
func TestLogin_RoundTrip(t *testing.T) {
	const secret = "test-session-secret"
	svc := NewService(fakeValidator{out: &gable.StaffValidation{
		StaffID:  "staff-1",
		Email:    "dispatcher@futurebuild.ai",
		Name:     "Dana Dispatcher",
		Entitled: true,
		Roles:    []string{"dispatcher"},
	}}, secret)

	resp, err := svc.Login(context.Background(), "dispatcher@futurebuild.ai")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if resp.Token == "" || resp.Name != "Dana Dispatcher" || len(resp.Roles) != 1 {
		t.Fatalf("unexpected login response: %+v", resp)
	}

	mw, err := middleware.NewAuthMiddleware(context.Background(), middleware.AuthConfig{
		SessionSecret: secret,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("middleware: %v", err)
	}

	var gotEmail string
	var gotRoles []string
	protected := mw.Handler(middleware.RequireRole("dispatcher")(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			c := middleware.ClaimsFromContext(r.Context())
			gotEmail = c.Email
			gotRoles = c.Roles
			w.WriteHeader(http.StatusOK)
		})))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflow/plans", nil)
	req.Header.Set("Authorization", "Bearer "+resp.Token)
	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from protected handler, got %d", rec.Code)
	}
	if gotEmail != "dispatcher@futurebuild.ai" {
		t.Fatalf("claims email not populated, got %q", gotEmail)
	}
	if len(gotRoles) != 1 || gotRoles[0] != "dispatcher" {
		t.Fatalf("claims roles not populated, got %v", gotRoles)
	}
}

// TestMiddleware_RejectsWrongSecret ensures a token signed with a different
// secret is rejected.
func TestMiddleware_RejectsWrongSecret(t *testing.T) {
	svc := NewService(fakeValidator{out: &gable.StaffValidation{
		StaffID: "s", Email: "e@x.ai", Name: "E", Entitled: true, Roles: []string{"yard"},
	}}, "real-secret")
	resp, err := svc.Login(context.Background(), "e@x.ai")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	mw, err := middleware.NewAuthMiddleware(context.Background(), middleware.AuthConfig{
		SessionSecret: "different-secret",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("middleware: %v", err)
	}

	called := false
	h := mw.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/profiles", nil)
	req.Header.Set("Authorization", "Bearer "+resp.Token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if called {
		t.Fatal("handler should not be reached with an invalid token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}
