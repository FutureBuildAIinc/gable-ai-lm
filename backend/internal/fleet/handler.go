// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package fleet

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/httputil"
)

// Handler exposes fleet-profile REST endpoints.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes registers fleet routes. roleGuard protects writes.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, roleGuard ...func(http.Handler) http.Handler) {
	guard := func(handler http.HandlerFunc) http.HandlerFunc {
		if len(roleGuard) > 0 && roleGuard[0] != nil {
			return func(w http.ResponseWriter, r *http.Request) {
				roleGuard[0](handler).ServeHTTP(w, r)
			}
		}
		return handler
	}

	mux.HandleFunc("GET /api/v1/fleet/profiles", guard(h.HandleListProfiles))
	mux.HandleFunc("GET /api/v1/fleet/profiles/{vehicleId}", guard(h.HandleGetProfile))
	mux.HandleFunc("PUT /api/v1/fleet/profiles/{vehicleId}", guard(h.HandleUpsertProfile))
}

func (h *Handler) HandleListProfiles(w http.ResponseWriter, r *http.Request) {
	profiles, err := h.svc.ListProfiles(r.Context())
	if err != nil {
		httputil.RespondError(w, r, "failed to list vehicle profiles", http.StatusInternalServerError, err)
		return
	}
	if profiles == nil {
		profiles = []Profile{}
	}
	httputil.RespondJSON(w, http.StatusOK, profiles)
}

func (h *Handler) HandleGetProfile(w http.ResponseWriter, r *http.Request) {
	vehicleID := r.PathValue("vehicleId")
	profile, err := h.svc.GetProfile(r.Context(), vehicleID)
	if errors.Is(err, ErrNotFound) {
		httputil.RespondError(w, r, "vehicle profile not found", http.StatusNotFound, err)
		return
	}
	if err != nil {
		httputil.RespondError(w, r, "failed to get vehicle profile", http.StatusInternalServerError, err)
		return
	}
	httputil.RespondJSON(w, http.StatusOK, profile)
}

func (h *Handler) HandleUpsertProfile(w http.ResponseWriter, r *http.Request) {
	vehicleID := r.PathValue("vehicleId")
	var in ProfileInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httputil.RespondError(w, r, "invalid request body", http.StatusBadRequest, err)
		return
	}
	profile, err := h.svc.UpsertProfile(r.Context(), vehicleID, in)
	if err != nil {
		httputil.RespondError(w, r, "failed to save vehicle profile", http.StatusInternalServerError, err)
		return
	}
	httputil.RespondJSON(w, http.StatusOK, profile)
}
