// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package httputil

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The error envelope carries two things that must not be confused: the
// sentence written for the person on the other end, and the diagnostic written
// for whoever reads the logs. Exactly one of them may be serialized.
//
// It used to serialize NEITHER — the caller's message was logged and then
// replaced on the wire with the HTTP status phrase. Every hand-written refusal
// in this service reached the browser as "Unprocessable Entity", including the
// dispatch gate's, and the app renders `error.message` verbatim.

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()
	var got ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("error envelope did not decode: %v (%s)", err, rec.Body.String())
	}
	return got
}

// TestRespondErrorDeliversTheCallersMessage pins the delivery half.
func TestRespondErrorDeliversTheCallersMessage(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		code int
		want string
	}{
		{
			name: "a dispatch-gate refusal reaches the dispatcher",
			msg:  "yard proof + sign-off required before depart on: Truck 1 - Flatbed",
			code: http.StatusUnprocessableEntity,
			want: "yard proof + sign-off required before depart on: Truck 1 - Flatbed",
		},
		{
			name: "a locked run tells the operator the cutoff it is asking approval for",
			msg:  "plan run is locked: morning run locked (cutoff 06:00) — re-assigning trucks requires manual approval (override)",
			code: http.StatusLocked,
			want: "(cutoff 06:00)",
		},
		{
			name: "a validation message names the field",
			msg:  `invalid request: invalid date "26-08-2026"; expected YYYY-MM-DD`,
			code: http.StatusBadRequest,
			want: "expected YYYY-MM-DD",
		},
		{
			// A caller with nothing specific to say must not send an empty
			// message — the status phrase is the floor, not the ceiling.
			name: "a blank message falls back to the status phrase",
			msg:  "   ",
			code: http.StatusNotFound,
			want: "Not Found",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/workflow/plans/x/push", nil)
			RespondError(rec, req, tc.msg, tc.code, errors.New("internal detail"))

			if rec.Code != tc.code {
				t.Fatalf("status = %d, want %d", rec.Code, tc.code)
			}
			got := decodeEnvelope(t, rec)
			if !strings.Contains(got.Error.Message, tc.want) {
				t.Errorf("client message = %q, want it to contain %q", got.Error.Message, tc.want)
			}
			if got.Error.Code != errorCode(tc.code) {
				t.Errorf("machine code = %q, want %q", got.Error.Code, errorCode(tc.code))
			}
		})
	}
}

// TestRespondErrorNeverSerializesTheDiagnostic pins the other half, which is
// the reason the substitution existed at all. The `err` argument routinely
// carries a driver error, a wrapped upstream URL, or a slice of somebody else's
// response body, and none of that may reach a browser.
func TestRespondErrorNeverSerializesTheDiagnostic(t *testing.T) {
	secrets := []string{
		`ERROR: column "gable_vehicle_id" does not exist (SQLSTATE 42703)`,
		`gable GET /api/integration/vehicles: status 500: {"stack":"main.go:412"}`,
		`dial tcp 10.0.3.7:5432: connect: connection refused`,
	}
	for _, secret := range secrets {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/profiles", nil)
		RespondError(rec, req, "failed to list vehicle profiles", http.StatusInternalServerError, errors.New(secret))

		body := rec.Body.String()
		if strings.Contains(body, secret) {
			t.Errorf("the diagnostic leaked into the response body: %s", body)
		}
		got := decodeEnvelope(t, rec)
		if got.Error.Message != "failed to list vehicle profiles" {
			t.Errorf("client message = %q", got.Error.Message)
		}
	}
}

// TestRespondErrorCarriesTheRequestID pins the correlation handle a support
// conversation runs on: the client is told which request to quote.
func TestRespondErrorCarriesTheRequestID(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Request-ID", "req-123")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/profiles", nil)
	RespondError(rec, req, "boom", http.StatusInternalServerError, nil)

	if got := decodeEnvelope(t, rec); got.Meta.RequestID != "req-123" {
		t.Errorf("request id = %q, want %q", got.Meta.RequestID, "req-123")
	}

	// Also from the inbound header, when the middleware has not stamped one.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/fleet/profiles", nil)
	req.Header.Set("X-Request-ID", "req-456")
	RespondError(rec, req, "boom", http.StatusInternalServerError, nil)
	if got := decodeEnvelope(t, rec); got.Meta.RequestID != "req-456" {
		t.Errorf("request id = %q, want %q", got.Meta.RequestID, "req-456")
	}
}
