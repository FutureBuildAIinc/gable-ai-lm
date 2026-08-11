// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package routing

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/httputil"
)

// Handler exposes routing REST endpoints.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes registers routing routes. roleGuard protects writes.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, roleGuard ...func(http.Handler) http.Handler) {
	guard := func(handler http.HandlerFunc) http.HandlerFunc {
		if len(roleGuard) > 0 && roleGuard[0] != nil {
			return func(w http.ResponseWriter, r *http.Request) {
				roleGuard[0](handler).ServeHTTP(w, r)
			}
		}
		return handler
	}

	mux.HandleFunc("POST /api/v1/routing/plan", guard(h.HandlePlan))
	mux.HandleFunc("GET /api/v1/routing/plan/{id}", guard(h.HandleGet))
	mux.HandleFunc("POST /api/v1/routing/plan/{id}/approve", guard(h.HandleApprove))
}

func (h *Handler) HandlePlan(w http.ResponseWriter, r *http.Request) {
	var req PlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, r, "invalid request body", http.StatusBadRequest, err)
		return
	}
	plan, err := h.svc.Plan(r.Context(), req)
	if err != nil {
		httputil.RespondError(w, r, "failed to build route plan", http.StatusBadGateway, err)
		return
	}
	httputil.RespondJSON(w, http.StatusOK, plan)
}

func (h *Handler) HandleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	plan, err := h.svc.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httputil.RespondError(w, r, "route plan not found", http.StatusNotFound, err)
		return
	}
	if err != nil {
		httputil.RespondError(w, r, "failed to get route plan", http.StatusInternalServerError, err)
		return
	}
	httputil.RespondJSON(w, http.StatusOK, plan)
}

func (h *Handler) HandleApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	plan, err := h.svc.Approve(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httputil.RespondError(w, r, "route plan not found", http.StatusNotFound, err)
		return
	}
	if err != nil {
		httputil.RespondError(w, r, "failed to approve route plan", http.StatusBadGateway, err)
		return
	}
	httputil.RespondJSON(w, http.StatusOK, plan)
}
