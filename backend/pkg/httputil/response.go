// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package httputil

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

// ErrorResponse is the standard JSON error envelope returned to clients.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
	Meta  ErrorMeta   `json:"meta"`
}

// ErrorDetail holds the machine-readable code and human-readable message.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrorMeta holds request-scoped metadata.
type ErrorMeta struct {
	RequestID string `json:"request_id"`
}

// RespondJSON writes v as a JSON response with the given status code.
func RespondJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// RespondError sends a structured JSON error to the client and logs the full
// error server-side with request context.
//
// The two arguments are for two different audiences and only one of them is
// ever serialized:
//
//   - msg is the CLIENT's sentence. Every caller writes it by hand, and it is
//     what the app renders (`aiLmService.jsonOrThrow` reads `error.message`
//     verbatim). Callers must therefore keep it free of schema, query and
//     internal identifiers — the same discipline they already apply, because
//     it is also the log line.
//   - err is the DIAGNOSTIC. It is logged and NEVER sent. That is what keeps
//     a pgx error, a wrapped upstream URL, or 512 bytes of another service's
//     response body out of a browser.
//
// msg used to be logged and then thrown away, replaced on the wire by
// genericMessage(code). The intent was leak prevention, and the effect was to
// silence a module whose whole job is refusing unsafe work out loud: a
// dispatcher blocked from sending an overweight truck was told "Unprocessable
// Entity", and a locked run's approval prompt lost the cutoff time it needed to
// ask for. The leak the substitution guarded against lives in err, which was
// never on the wire in the first place.
//
// A blank msg still falls back to the generic phrase, so a caller that has
// nothing specific to say cannot accidentally send an empty message.
func RespondError(w http.ResponseWriter, r *http.Request, msg string, code int, err error) {
	RespondCodedError(w, r, errorCode(code), msg, code, err)
}

// RespondCodedError is RespondError with the machine-readable code chosen by
// the caller instead of derived from the status.
//
// It exists because a status is not always specific enough to act on, and the
// client cannot be asked to recover by matching prose. Two different refusals
// in this service answer 409 Conflict — "somebody else edited this record,
// reload before you retry" and "somebody else is holding this dispatch date,
// nothing happened, just try again" — and they need OPPOSITE things from the
// operator. Shipping both as CONFLICT left the app rendering one hardcoded
// banner for both, telling a dispatcher whose change was never applied that
// their change had been overwritten.
//
// code is a stable identifier the client switches on (DATE_BUSY), not a second
// message. Keep it SCREAMING_SNAKE and keep it out of the sentence.
func RespondCodedError(w http.ResponseWriter, r *http.Request, code, msg string, status int, err error) {
	reqID := w.Header().Get("X-Request-ID")
	if reqID == "" {
		reqID = r.Header.Get("X-Request-ID")
	}

	slog.Error(msg,
		"error", err,
		"status", status,
		"code", code,
		"method", r.Method,
		"path", r.URL.Path,
		"request_id", reqID,
	)

	clientMsg := strings.TrimSpace(msg)
	if clientMsg == "" {
		clientMsg = genericMessage(status)
	}
	if code == "" {
		code = errorCode(status)
	}

	RespondJSON(w, status, ErrorResponse{
		Error: ErrorDetail{Code: code, Message: clientMsg},
		Meta:  ErrorMeta{RequestID: reqID},
	})
}

func errorCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "BAD_REQUEST"
	case http.StatusUnauthorized:
		return "UNAUTHORIZED"
	case http.StatusForbidden:
		return "FORBIDDEN"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusConflict:
		return "CONFLICT"
	case http.StatusTooManyRequests:
		return "RATE_LIMITED"
	case http.StatusUnprocessableEntity:
		return "UNPROCESSABLE_ENTITY"
	case http.StatusBadGateway:
		return "UPSTREAM_ERROR"
	default:
		if status >= 400 && status < 500 {
			return "BAD_REQUEST"
		}
		return "INTERNAL_ERROR"
	}
}

func genericMessage(code int) string {
	switch code {
	case http.StatusNotFound:
		return "Not Found"
	case http.StatusForbidden:
		return "Forbidden"
	case http.StatusUnauthorized:
		return "Unauthorized"
	case http.StatusConflict:
		return "Conflict"
	case http.StatusTooManyRequests:
		return "Too Many Requests"
	case http.StatusUnprocessableEntity:
		return "Unprocessable Entity"
	case http.StatusBadGateway:
		return "Upstream Service Error"
	default:
		if code >= 400 && code < 500 {
			return "Bad Request"
		}
		return "Internal Server Error"
	}
}
