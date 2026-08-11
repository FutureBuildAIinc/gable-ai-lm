// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package catalog

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/httputil"
)

// Handler exposes product-dimension REST endpoints.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes registers catalog routes. roleGuard protects writes.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, roleGuard ...func(http.Handler) http.Handler) {
	guard := func(handler http.HandlerFunc) http.HandlerFunc {
		if len(roleGuard) > 0 && roleGuard[0] != nil {
			return func(w http.ResponseWriter, r *http.Request) {
				roleGuard[0](handler).ServeHTTP(w, r)
			}
		}
		return handler
	}

	mux.HandleFunc("GET /api/v1/catalog/products", guard(h.HandleListProducts))
	mux.HandleFunc("GET /api/v1/catalog/dimensions", guard(h.HandleListDimensions))
	mux.HandleFunc("GET /api/v1/catalog/dimensions/{productId}", guard(h.HandleGetDimension))
	mux.HandleFunc("PUT /api/v1/catalog/dimensions/{productId}", guard(h.HandleUpsertDimension))
}

// HandleListProducts returns the resolved load-planning catalog (PIM geometry
// merged with AI_LM overrides) for the Load Builder. A failure to reach the PIM
// is surfaced as 502 Bad Gateway so the UI can distinguish an upstream outage
// from a legitimately empty catalog.
func (h *Handler) HandleListProducts(w http.ResponseWriter, r *http.Request) {
	products, err := h.svc.ListEffectiveProducts(r.Context())
	if err != nil {
		httputil.RespondError(w, r, "failed to load catalog from PIM", http.StatusBadGateway, err)
		return
	}
	if products == nil {
		products = []EffectiveProduct{}
	}
	httputil.RespondJSON(w, http.StatusOK, products)
}

func (h *Handler) HandleListDimensions(w http.ResponseWriter, r *http.Request) {
	dims, err := h.svc.ListDimensions(r.Context())
	if err != nil {
		httputil.RespondError(w, r, "failed to list product dimensions", http.StatusInternalServerError, err)
		return
	}
	if dims == nil {
		dims = []Dimension{}
	}
	httputil.RespondJSON(w, http.StatusOK, dims)
}

func (h *Handler) HandleGetDimension(w http.ResponseWriter, r *http.Request) {
	productID := r.PathValue("productId")
	dim, err := h.svc.GetDimension(r.Context(), productID)
	if errors.Is(err, ErrNotFound) {
		httputil.RespondError(w, r, "product dimensions not found", http.StatusNotFound, err)
		return
	}
	if err != nil {
		httputil.RespondError(w, r, "failed to get product dimensions", http.StatusInternalServerError, err)
		return
	}
	httputil.RespondJSON(w, http.StatusOK, dim)
}

func (h *Handler) HandleUpsertDimension(w http.ResponseWriter, r *http.Request) {
	productID := r.PathValue("productId")
	var in DimensionInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httputil.RespondError(w, r, "invalid request body", http.StatusBadRequest, err)
		return
	}
	dim, err := h.svc.UpsertDimension(r.Context(), productID, in)
	if err != nil {
		httputil.RespondError(w, r, "failed to save product dimensions", http.StatusInternalServerError, err)
		return
	}
	httputil.RespondJSON(w, http.StatusOK, dim)
}
