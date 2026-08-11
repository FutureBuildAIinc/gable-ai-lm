// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNotConfigured verifies the graceful-degradation contract: no key ⇒ not
// configured ⇒ Generate returns ErrNotConfigured (never panics, never calls out).
func TestNotConfigured(t *testing.T) {
	c := NewClient("", "", "")
	if c.Configured() {
		t.Fatal("client with empty key should report not configured")
	}
	if _, err := c.Generate(context.Background(), "sys", "user", 100); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}
}

// TestDefaults verifies an empty base URL/model fall back to the OSS defaults.
func TestDefaults(t *testing.T) {
	c := NewClient("k", "", "")
	if c.Model() == "" {
		t.Fatal("expected a default open-weight model id")
	}
	if c.baseURL != defaultBaseURL {
		t.Fatalf("expected default base URL, got %s", c.baseURL)
	}
}

// TestGenerateHappyPath verifies the OpenAI-compatible request/response wiring.
func TestGenerateHappyPath(t *testing.T) {
	var gotAuth, gotPath string
	var gotReq chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		resp := chatResponse{Model: "test-model"}
		resp.Choices = append(resp.Choices, struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		}{})
		resp.Choices[0].Message.Content = "  Dispatch looks clear.  "
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClient("secret", srv.URL, "my-model")
	out, err := c.Generate(context.Background(), "system text", "user text", 256)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "Dispatch looks clear." {
		t.Fatalf("content not trimmed/returned: %q", out)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("expected Bearer auth, got %q", gotAuth)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("unexpected path %q", gotPath)
	}
	if gotReq.Model != "my-model" || len(gotReq.Messages) != 2 {
		t.Errorf("unexpected request shape: %+v", gotReq)
	}
}

// attributionServer returns a server that records the attribution headers of
// the request it receives, plus a getter for them.
func attributionServer(t *testing.T) (*httptest.Server, func() (referer, title string)) {
	t.Helper()
	var referer, title string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		referer = r.Header.Get("HTTP-Referer")
		title = r.Header.Get("X-Title")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() (string, string) { return referer, title }
}

// TestAttributionOmittedByDefault is the productization contract: an install
// that configures nothing must not report its inference traffic under any
// vendor's identity. The headers used to be hardcoded to this repository.
func TestAttributionOmittedByDefault(t *testing.T) {
	t.Setenv(envAppURL, "")
	t.Setenv(envAppTitle, "")
	srv, got := attributionServer(t)

	c := NewClient("secret", srv.URL, "m")
	if a := c.Attribution(); !a.empty() {
		t.Fatalf("expected no attribution by default, got %+v", a)
	}
	if _, err := c.Generate(context.Background(), "sys", "user", 16); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	referer, title := got()
	if referer != "" || title != "" {
		t.Fatalf("expected no attribution headers, got referer=%q title=%q", referer, title)
	}
}

// TestAttributionFromEnv verifies an operator can opt in without a code change.
func TestAttributionFromEnv(t *testing.T) {
	t.Setenv(envAppURL, "https://dispatch.yourdealer.example")
	t.Setenv(envAppTitle, "Yourdealer Dispatch")
	srv, got := attributionServer(t)

	c := NewClient("secret", srv.URL, "m")
	if _, err := c.Generate(context.Background(), "sys", "user", 16); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	referer, title := got()
	if referer != "https://dispatch.yourdealer.example" {
		t.Errorf("HTTP-Referer = %q", referer)
	}
	if title != "Yourdealer Dispatch" {
		t.Errorf("X-Title = %q", title)
	}
}

// TestWithAttributionOverridesEnv verifies the config seam wins over ambient
// environment, so a caller wiring attribution from app config is authoritative.
func TestWithAttributionOverridesEnv(t *testing.T) {
	t.Setenv(envAppURL, "https://from-env.example")
	t.Setenv(envAppTitle, "From env")
	srv, got := attributionServer(t)

	c := NewClient("secret", srv.URL, "m", WithAttribution(Attribution{
		URL:   "  https://from-config.example  ",
		Title: "  From config  ",
	}))
	if _, err := c.Generate(context.Background(), "sys", "user", 16); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	referer, title := got()
	if referer != "https://from-config.example" || title != "From config" {
		t.Fatalf("config attribution not applied/trimmed: referer=%q title=%q", referer, title)
	}
}

// TestPartialAttributionSendsOnlyWhatIsSet keeps a half-configured operator
// from emitting an empty header.
func TestPartialAttributionSendsOnlyWhatIsSet(t *testing.T) {
	t.Setenv(envAppURL, "")
	t.Setenv(envAppTitle, "")
	srv, got := attributionServer(t)

	c := NewClient("secret", srv.URL, "m", WithAttribution(Attribution{Title: "Yard AI"}))
	if _, err := c.Generate(context.Background(), "sys", "user", 16); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	referer, title := got()
	if referer != "" {
		t.Errorf("expected no HTTP-Referer, got %q", referer)
	}
	if title != "Yard AI" {
		t.Errorf("X-Title = %q", title)
	}
}
