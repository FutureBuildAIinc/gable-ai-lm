// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package compliance

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/httputil"
)

// Handler exposes compliance REST endpoints.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes registers compliance routes. roleGuard protects writes.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, roleGuard ...func(http.Handler) http.Handler) {
	guard := func(handler http.HandlerFunc) http.HandlerFunc {
		if len(roleGuard) > 0 && roleGuard[0] != nil {
			return func(w http.ResponseWriter, r *http.Request) {
				roleGuard[0](handler).ServeHTTP(w, r)
			}
		}
		return handler
	}

	mux.HandleFunc("POST /api/v1/compliance/check-route", guard(h.HandleCheckRoute))
	mux.HandleFunc("GET /api/v1/compliance/restricted-points", guard(h.HandleListPoints))
	mux.HandleFunc("POST /api/v1/compliance/restricted-points", guard(h.HandleCreatePoint))
	mux.HandleFunc("PUT /api/v1/compliance/restricted-points/{id}", guard(h.HandleUpdatePoint))
}

func (h *Handler) HandleCheckRoute(w http.ResponseWriter, r *http.Request) {
	var req RouteCheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, r, "invalid request body", http.StatusBadRequest, err)
		return
	}
	result, err := h.svc.CheckRoute(r.Context(), req)
	if err != nil {
		httputil.RespondError(w, r, "failed to check route", http.StatusInternalServerError, err)
		return
	}
	httputil.RespondJSON(w, http.StatusOK, result)
}

func (h *Handler) HandleListPoints(w http.ResponseWriter, r *http.Request) {
	points, err := h.svc.ListPoints(r.Context())
	if err != nil {
		httputil.RespondError(w, r, "failed to list restricted points", http.StatusInternalServerError, err)
		return
	}
	if points == nil {
		points = []RestrictedPoint{}
	}
	httputil.RespondJSON(w, http.StatusOK, points)
}

func (h *Handler) HandleCreatePoint(w http.ResponseWriter, r *http.Request) {
	var in RestrictedPointInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httputil.RespondError(w, r, "invalid request body", http.StatusBadRequest, err)
		return
	}
	point, err := h.svc.CreatePoint(r.Context(), in)
	if err != nil {
		httputil.RespondError(w, r, "failed to create restricted point", http.StatusInternalServerError, err)
		return
	}
	httputil.RespondJSON(w, http.StatusCreated, point)
}

func (h *Handler) HandleUpdatePoint(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in RestrictedPointInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httputil.RespondError(w, r, "invalid request body", http.StatusBadRequest, err)
		return
	}
	point, err := h.svc.UpdatePoint(r.Context(), id, in)
	if errors.Is(err, ErrNotFound) {
		httputil.RespondError(w, r, "restricted point not found", http.StatusNotFound, err)
		return
	}
	if err != nil {
		httputil.RespondError(w, r, "failed to update restricted point", http.StatusInternalServerError, err)
		return
	}
	httputil.RespondJSON(w, http.StatusOK, point)
}
