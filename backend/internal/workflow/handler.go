// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/httputil"
)

// Handler exposes the guided workflow REST endpoints.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes registers workflow routes. roleGuard protects writes.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, roleGuard ...func(http.Handler) http.Handler) {
	guard := func(handler http.HandlerFunc) http.HandlerFunc {
		if len(roleGuard) > 0 && roleGuard[0] != nil {
			return func(w http.ResponseWriter, r *http.Request) {
				roleGuard[0](handler).ServeHTTP(w, r)
			}
		}
		return handler
	}

	mux.HandleFunc("POST /api/v1/workflow/plans", guard(h.HandleIngest))
	mux.HandleFunc("GET /api/v1/workflow/plans/latest", guard(h.HandleLatest))
	mux.HandleFunc("GET /api/v1/workflow/plans/{id}", guard(h.HandleGet))
	mux.HandleFunc("POST /api/v1/workflow/plans/{id}/assign", guard(h.HandleAssign))
	mux.HandleFunc("POST /api/v1/workflow/plans/{id}/pack", guard(h.HandlePack))
	mux.HandleFunc("PUT /api/v1/workflow/plans/{id}/loads/{vehicleId}/sequence", guard(h.HandleResequence))
	mux.HandleFunc("PUT /api/v1/workflow/plans/{id}/stops/{orderId}/priority", guard(h.HandlePriority))
	mux.HandleFunc("PUT /api/v1/workflow/plans/{id}/orders/{orderId}/dimensions", guard(h.HandleSetDimensions))
	mux.HandleFunc("POST /api/v1/workflow/plans/{id}/review", guard(h.HandleReview))
	mux.HandleFunc("POST /api/v1/workflow/plans/{id}/push", guard(h.HandlePush))
	mux.HandleFunc("GET /api/v1/workflow/plans/{id}/briefing", guard(h.HandleBriefing))

	// Proof-of-load + sign-off (T1-6).
	mux.HandleFunc("POST /api/v1/workflow/plans/{id}/loads/{vehicleId}/proof", guard(h.HandleAttachProof))
	mux.HandleFunc("POST /api/v1/workflow/plans/{id}/loads/{vehicleId}/sign-off", guard(h.HandleSignOff))

	// Scheduled re-optimization windows + lock states (T2-3).
	mux.HandleFunc("POST /api/v1/workflow/plans/{id}/lock", guard(h.HandleLock))
	mux.HandleFunc("POST /api/v1/workflow/plans/{id}/unlock", guard(h.HandleUnlock))
	mux.HandleFunc("POST /api/v1/workflow/plans/{id}/late-adds", guard(h.HandleLateAdd))
	mux.HandleFunc("POST /api/v1/workflow/plans/{id}/late-adds/{orderId}/resolve", guard(h.HandleResolveLateAdd))
}

func (h *Handler) HandleIngest(w http.ResponseWriter, r *http.Request) {
	var req IngestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, r, "invalid request body", http.StatusBadRequest, err)
		return
	}
	plan, err := h.svc.Ingest(r.Context(), req)
	if err != nil {
		httputil.RespondError(w, r, "ingest failed", http.StatusBadGateway, err)
		return
	}
	httputil.RespondJSON(w, http.StatusCreated, plan)
}

func (h *Handler) HandleLatest(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	if date == "" {
		httputil.RespondError(w, r, "date query parameter required", http.StatusBadRequest, nil)
		return
	}
	plan, err := h.svc.GetLatestForDate(r.Context(), date)
	if errors.Is(err, ErrNotFound) {
		httputil.RespondError(w, r, "no plan for date", http.StatusNotFound, err)
		return
	}
	if err != nil {
		httputil.RespondError(w, r, "lookup failed", http.StatusInternalServerError, err)
		return
	}
	httputil.RespondJSON(w, http.StatusOK, plan)
}

func (h *Handler) HandleGet(w http.ResponseWriter, r *http.Request) {
	plan, err := h.svc.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		httputil.RespondError(w, r, "plan not found", http.StatusNotFound, err)
		return
	}
	if err != nil {
		httputil.RespondError(w, r, "lookup failed", http.StatusInternalServerError, err)
		return
	}
	httputil.RespondJSON(w, http.StatusOK, plan)
}

// HandleAssign runs (or re-runs) truck assignment. It tolerates an empty body;
// when present it carries the override (manual approval) for a locked run (T2-3).
func (h *Handler) HandleAssign(w http.ResponseWriter, r *http.Request) {
	var req AssignRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // body optional
	plan, err := h.svc.Assign(r.Context(), r.PathValue("id"), req.Override, req.ApprovedBy)
	h.respondStep(w, r, plan, err)
}

func (h *Handler) HandlePack(w http.ResponseWriter, r *http.Request) {
	h.step(w, r, h.svc.Pack)
}

func (h *Handler) HandleReview(w http.ResponseWriter, r *http.Request) {
	h.step(w, r, h.svc.Review)
}

func (h *Handler) HandlePush(w http.ResponseWriter, r *http.Request) {
	h.step(w, r, h.svc.Push)
}

// step runs one id-keyed workflow transition with shared error mapping.
func (h *Handler) step(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, id string) (*Plan, error)) {
	plan, err := fn(r.Context(), r.PathValue("id"))
	h.respondStep(w, r, plan, err)
}

// respondStep is the shared success/error mapping for workflow transitions. A
// locked run (T2-3) maps to 423 Locked so the UI can prompt for approval.
func (h *Handler) respondStep(w http.ResponseWriter, r *http.Request, plan *Plan, err error) {
	if errors.Is(err, ErrNotFound) {
		httputil.RespondError(w, r, "plan not found", http.StatusNotFound, err)
		return
	}
	if errors.Is(err, ErrLocked) {
		httputil.RespondError(w, r, err.Error(), http.StatusLocked, err)
		return
	}
	// Someone else saved this plan between our read and our write. Nothing was
	// applied — 409 tells the UI to reload the current plan and retry.
	if errors.Is(err, ErrVersionConflict) {
		httputil.RespondError(w, r, err.Error(), http.StatusConflict, err)
		return
	}
	if err != nil {
		httputil.RespondError(w, r, "workflow step failed", http.StatusUnprocessableEntity, err)
		return
	}
	httputil.RespondJSON(w, http.StatusOK, plan)
}

func (h *Handler) HandleResequence(w http.ResponseWriter, r *http.Request) {
	var req ResequenceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, r, "invalid request body", http.StatusBadRequest, err)
		return
	}
	plan, err := h.svc.Resequence(r.Context(), r.PathValue("id"), r.PathValue("vehicleId"), req.OrderIDs, req.Override, req.ApprovedBy)
	h.respondStep(w, r, plan, err)
}

// HandlePriority toggles an order's deliver-first flag (T2-1) and re-sequences
// the affected truck, pinning priority stops to the front of the route.
func (h *Handler) HandlePriority(w http.ResponseWriter, r *http.Request) {
	var req PriorityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, r, "invalid request body", http.StatusBadRequest, err)
		return
	}
	plan, err := h.svc.SetPriority(r.Context(), r.PathValue("id"), r.PathValue("orderId"), req.Priority, req.Override, req.ApprovedBy)
	h.respondStep(w, r, plan, err)
}

// HandleSetDimensions applies a per-order dimension override for a variable-
// dimension SKU (T2-2) and re-packs the affected truck.
func (h *Handler) HandleSetDimensions(w http.ResponseWriter, r *http.Request) {
	var req DimensionOverrideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, r, "invalid request body", http.StatusBadRequest, err)
		return
	}
	plan, err := h.svc.SetLineDimensions(r.Context(), r.PathValue("id"), r.PathValue("orderId"), req)
	if errors.Is(err, ErrNotFound) {
		httputil.RespondError(w, r, "plan not found", http.StatusNotFound, err)
		return
	}
	if errors.Is(err, ErrVersionConflict) {
		httputil.RespondError(w, r, err.Error(), http.StatusConflict, err)
		return
	}
	if err != nil {
		httputil.RespondError(w, r, "set dimensions failed", http.StatusUnprocessableEntity, err)
		return
	}
	httputil.RespondJSON(w, http.StatusOK, plan)
}

// HandleAttachProof records a yard photo/video reference on a load (T1-6).
func (h *Handler) HandleAttachProof(w http.ResponseWriter, r *http.Request) {
	var req ProofRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, r, "invalid request body", http.StatusBadRequest, err)
		return
	}
	plan, err := h.svc.AttachProof(r.Context(), r.PathValue("id"), r.PathValue("vehicleId"), req)
	h.respondStep(w, r, plan, err)
}

// HandleSignOff records the yard sign-off that releases a load to depart (T1-6).
func (h *Handler) HandleSignOff(w http.ResponseWriter, r *http.Request) {
	var req SignOffRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, r, "invalid request body", http.StatusBadRequest, err)
		return
	}
	plan, err := h.svc.SignOffLoad(r.Context(), r.PathValue("id"), r.PathValue("vehicleId"), req)
	h.respondStep(w, r, plan, err)
}

// HandleLock sets a run's lock / scheduled-lock state (T2-3).
func (h *Handler) HandleLock(w http.ResponseWriter, r *http.Request) {
	var req LockRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, r, "invalid request body", http.StatusBadRequest, err)
		return
	}
	plan, err := h.svc.SetLock(r.Context(), r.PathValue("id"), req)
	h.respondStep(w, r, plan, err)
}

// HandleUnlock clears a run's lock so it can be re-optimized (T2-3).
func (h *Handler) HandleUnlock(w http.ResponseWriter, r *http.Request) {
	var req LockRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // body optional
	plan, err := h.svc.Unlock(r.Context(), r.PathValue("id"), req.Reason, req.LockedBy)
	h.respondStep(w, r, plan, err)
}

// HandleLateAdd queues a late same-day order onto a run (T2-3).
func (h *Handler) HandleLateAdd(w http.ResponseWriter, r *http.Request) {
	var req LateAddRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, r, "invalid request body", http.StatusBadRequest, err)
		return
	}
	plan, err := h.svc.AddLateOrder(r.Context(), r.PathValue("id"), req)
	h.respondStep(w, r, plan, err)
}

// HandleResolveLateAdd approves or rejects a queued late add (T2-3).
func (h *Handler) HandleResolveLateAdd(w http.ResponseWriter, r *http.Request) {
	var req LateAddApproveRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // body optional
	plan, err := h.svc.ResolveLateAdd(r.Context(), r.PathValue("id"), r.PathValue("orderId"), req)
	h.respondStep(w, r, plan, err)
}

// HandleBriefing returns the LLM dispatch briefing for a plan. It always responds
// 200: when AI is unconfigured the payload reports availability=false with a hint.
func (h *Handler) HandleBriefing(w http.ResponseWriter, r *http.Request) {
	briefing, err := h.svc.Briefing(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		httputil.RespondError(w, r, "plan not found", http.StatusNotFound, err)
		return
	}
	if err != nil {
		httputil.RespondError(w, r, "briefing failed", http.StatusInternalServerError, err)
		return
	}
	httputil.RespondJSON(w, http.StatusOK, briefing)
}
