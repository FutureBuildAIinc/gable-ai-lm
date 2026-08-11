// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package load

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/httputil"
)

// Handler exposes load-optimization REST endpoints.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes registers load routes. roleGuard protects the optimize write.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, roleGuard ...func(http.Handler) http.Handler) {
	guard := func(handler http.HandlerFunc) http.HandlerFunc {
		if len(roleGuard) > 0 && roleGuard[0] != nil {
			return func(w http.ResponseWriter, r *http.Request) {
				roleGuard[0](handler).ServeHTTP(w, r)
			}
		}
		return handler
	}

	mux.HandleFunc("POST /api/v1/load/optimize", guard(h.HandleOptimize))
	mux.HandleFunc("GET /api/v1/load/{id}", guard(h.HandleGet))
}

func (h *Handler) HandleOptimize(w http.ResponseWriter, r *http.Request) {
	var req OptimizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, r, "invalid request body", http.StatusBadRequest, err)
		return
	}
	plan, err := h.svc.Optimize(r.Context(), req)
	if err != nil {
		httputil.RespondError(w, r, "failed to optimize load", http.StatusUnprocessableEntity, err)
		return
	}
	httputil.RespondJSON(w, http.StatusOK, plan)
}

func (h *Handler) HandleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	plan, err := h.svc.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httputil.RespondError(w, r, "load plan not found", http.StatusNotFound, err)
		return
	}
	if err != nil {
		httputil.RespondError(w, r, "failed to get load plan", http.StatusInternalServerError, err)
		return
	}
	httputil.RespondJSON(w, http.StatusOK, plan)
}
