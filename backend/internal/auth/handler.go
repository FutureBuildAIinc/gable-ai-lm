// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package auth

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/httputil"
)

// Handler exposes the public staff-login endpoint.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes wires the public login route. It must NOT be behind the
// role/auth guard — it is how a session is obtained in the first place (the
// path is registered as a public path on the auth middleware).
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/auth/login", h.HandleLogin)
}

func (h *Handler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondMessage(w, r, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	resp, err := h.svc.Login(r.Context(), req.Email)
	switch {
	case errors.Is(err, ErrEmailRequired):
		respondMessage(w, r, http.StatusBadRequest, "BAD_REQUEST", ErrEmailRequired.Error())
		return
	case errors.Is(err, ErrNotEntitled):
		// Authenticated identity, but no AI_LM grant.
		respondMessage(w, r, http.StatusForbidden, "FORBIDDEN", ErrNotEntitled.Error())
		return
	case err != nil:
		// Upstream (GableLBM) failure or signing error — log detail, hide it.
		httputil.RespondError(w, r, "login failed", http.StatusBadGateway, err)
		return
	}

	httputil.RespondJSON(w, http.StatusOK, resp)
}

// respondMessage emits the standard error envelope but with a client-facing
// message preserved (httputil.RespondError intentionally genericizes messages;
// login needs the user to see "not authorized for AI_LM").
func respondMessage(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	reqID := w.Header().Get("X-Request-ID")
	if reqID == "" {
		reqID = r.Header.Get("X-Request-ID")
	}
	httputil.RespondJSON(w, status, httputil.ErrorResponse{
		Error: httputil.ErrorDetail{Code: code, Message: msg},
		Meta:  httputil.ErrorMeta{RequestID: reqID},
	})
}
