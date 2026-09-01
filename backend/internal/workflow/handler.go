// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/FutureBuildAIinc/gable-ai-lm/pkg/httputil"
)

// CodeDateBusy is the machine-readable error code for "another writer holds
// this dispatch date". It rides on a 409 alongside the version-conflict 409 and
// is what tells the two apart on the wire.
//
// The distinction is not cosmetic: a version conflict means somebody changed
// the record under you and the only safe recovery is RELOAD, while a busy date
// means nothing was applied at all and the recovery is simply to REPEAT. A
// client that cannot tell them apart must guess, and the app guessed wrong for
// every busy date it ever saw.
const CodeDateBusy = "DATE_BUSY"

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
	// The dispatch board as GableLBM actually holds it, reconciled against this
	// service's ledgers. Read-only: it is the surface an orphaned route is
	// SURFACED on, and a surface that repaired things could not be polled.
	mux.HandleFunc("GET /api/v1/workflow/dispatch-board", guard(h.HandleDispatchBoard))
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

// HandleIngest starts a workflow run for a date.
//
// The two failure modes are reported differently on purpose. A bad request —
// empty body, missing date, unparseable date — is the CLIENT's mistake and maps
// to 400; only a genuine failure to reach or read GableLBM (or the catalog it
// serves) maps to 502 UPSTREAM_ERROR. Mapping every ingest error to 502 told
// operators "GableLBM is down" whenever a caller posted an empty body, which is
// a support call spent on a healthy ERP.
func (h *Handler) HandleIngest(w http.ResponseWriter, r *http.Request) {
	var req IngestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		// io.EOF is an empty body: let Ingest's own validation name the missing
		// field rather than reporting it as malformed JSON.
		httputil.RespondError(w, r, "invalid request body", http.StatusBadRequest, err)
		return
	}
	plan, err := h.svc.Ingest(r.Context(), req)
	if errors.Is(err, ErrInvalidRequest) {
		httputil.RespondError(w, r, err.Error(), http.StatusBadRequest, err)
		return
	}
	// Re-planning a date whose previous plan still has routes on the dispatch
	// board needs an approver, and answers 423 with its own sentence — the same
	// prompt the lock and the assign/pack gates open. A 502 here would tell the
	// dispatcher GableLBM was down, when in fact it is holding live routes that
	// somebody has to agree to withdraw.
	if errors.Is(err, ErrPushed) {
		httputil.RespondError(w, r, err.Error(), http.StatusLocked, err)
		return
	}
	// The dispatch board could not be read, so this re-plan FAILED CLOSED. It
	// is 502 and not 423 on purpose: 423 opens the approval prompt, and there
	// is nothing here a dispatcher could approve — approving would mean
	// "re-plan over a board you cannot see", which is the harm itself. It is
	// also not the generic "ingest failed" 502 below, because "GableLBM is
	// unreachable, nothing changed, try again" is a different support call from
	// "the catalog would not resolve".
	if errors.Is(err, ErrBoardUnreadable) {
		httputil.RespondError(w, r, err.Error(), http.StatusBadGateway, err)
		return
	}
	// A truck that has already left the yard cannot have its route recalled, so
	// the re-plan is refused with the sentence naming it (422), not a shrug.
	var refusal *Refusal
	if errors.As(err, &refusal) {
		httputil.RespondError(w, r, refusal.Msg, http.StatusUnprocessableEntity, err)
		return
	}
	// Another writer holds this dispatch date: a push, a re-assignment or
	// another re-plan. Nothing was recalled and no plan was created, so the
	// dispatcher waits and repeats — see respondStep for why this 409 carries
	// its own code.
	if errors.Is(err, ErrDateBusy) {
		httputil.RespondCodedError(w, r, CodeDateBusy, err.Error(), http.StatusConflict, err)
		return
	}
	// Someone else wrote the superseded plan between our read and our write.
	if errors.Is(err, ErrVersionConflict) {
		httputil.RespondError(w, r, err.Error(), http.StatusConflict, err)
		return
	}
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

// HandleDispatchBoard reports what GableLBM's dispatch board holds for a date
// and every way this service's ledgers disagree with it.
//
// It exists because "surface an orphan, do not silently recall it" needs a
// place to surface it. The re-plan gate is what actually closes the harm — a
// re-ingest can no longer plan over a route it cannot see — but a 423 only
// reaches somebody who happened to attempt a re-plan, and a route on the
// dealer's board that no plan owns has to be findable on its own by a human who
// can decide what to do about it.
//
// It writes NOTHING, including the ghost claims it reports. Ghost repair runs
// on the re-plan path under the dispatch-date claim that makes it safe; doing
// it here would mean a GET that mutates, which cannot be polled or put on a
// dashboard, and would change state for a caller who only asked a question.
//
// date is required — a dateless board is not a smaller question, it is a
// meaningless one, exactly as it is on the ERP endpoint underneath.
func (h *Handler) HandleDispatchBoard(w http.ResponseWriter, r *http.Request) {
	report, err := h.svc.BoardReportForDate(r.Context(), r.URL.Query().Get("date"))
	if errors.Is(err, ErrInvalidRequest) {
		httputil.RespondError(w, r, err.Error(), http.StatusBadRequest, err)
		return
	}
	// Unreadable is reported as unreadable. Answering 200 with an empty board
	// would tell a dashboard the date is clean at the exact moment nothing is
	// known about it — the same fail-open this whole change removes.
	if errors.Is(err, ErrBoardUnreadable) {
		httputil.RespondError(w, r, err.Error(), http.StatusBadGateway, err)
		return
	}
	if err != nil {
		httputil.RespondError(w, r, "dispatch board lookup failed", http.StatusInternalServerError, err)
		return
	}
	httputil.RespondJSON(w, http.StatusOK, report)
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

// HandlePack runs (or re-runs) 3D packing. Like HandleAssign it tolerates an
// empty body; when present it carries the override (manual approval) for a
// locked run or for one whose routes are already live on the dispatch board.
func (h *Handler) HandlePack(w http.ResponseWriter, r *http.Request) {
	var req PackRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // body optional
	plan, err := h.svc.Pack(r.Context(), r.PathValue("id"), req.Override, req.ApprovedBy)
	h.respondStep(w, r, plan, err)
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
//
// A *Refusal — a gate this module closed on its own rules — is answered with
// its OWN sentence. That sentence is the product: the dispatch gate names the
// truck and the reason it will not go ("yard proof + sign-off required before
// depart on: Truck 1 - Flatbed"), and until this branch existed every one of
// them collapsed into a bare "workflow step failed" that the operator never saw
// anyway. Anything else stays generic here, because a wrapped upstream error
// carries a URL and up to 512 bytes of GableLBM's response body.
func (h *Handler) respondStep(w http.ResponseWriter, r *http.Request, plan *Plan, err error) {
	if errors.Is(err, ErrNotFound) {
		httputil.RespondError(w, r, "plan not found", http.StatusNotFound, err)
		return
	}
	// A locked run (T2-3) and a plan whose routes are already live on the
	// dispatch board are the same conversation with the dispatcher: "this needs
	// an approver". Both carry their own sentence and both answer 423, so the
	// UI has exactly one override prompt to implement.
	if errors.Is(err, ErrLocked) || errors.Is(err, ErrPushed) {
		httputil.RespondError(w, r, err.Error(), http.StatusLocked, err)
		return
	}
	// Another writer for the same dispatch date holds it. Nothing was gated,
	// sent or written, so this is a plain "wait and resubmit" — the same status
	// as a version conflict, and the OPPOSITE instruction.
	//
	// That is why it carries its own error CODE. Both are 409, and the app
	// keyed its banner off the status alone: every 409 rendered "Someone else
	// changed this plan while you were working... Reload to see theirs, then
	// redo yours", which for a busy date is false in all three of its claims —
	// nobody changed the plan, nothing was applied to reload past, and
	// reloading fixes nothing. Telling the two apart "by the sentence" was
	// never possible for a client that never showed the sentence.
	if errors.Is(err, ErrDateBusy) {
		httputil.RespondCodedError(w, r, CodeDateBusy, err.Error(), http.StatusConflict, err)
		return
	}
	// Someone else saved this plan between our read and our write. Nothing was
	// applied — 409 tells the UI to reload the current plan and retry.
	if errors.Is(err, ErrVersionConflict) {
		httputil.RespondError(w, r, err.Error(), http.StatusConflict, err)
		return
	}
	// The dispatch board could not be read, so this step FAILED CLOSED — the
	// same 502, with the same sentence, that HandleIngest answers. Assign reads
	// the board now, and without this its refusal fell through to the generic
	// "workflow step failed" 422 below: an operator would be told the request
	// was unprocessable when in fact GableLBM was unreachable and nothing had
	// been attempted. It is deliberately NOT 423 — approving would mean
	// "re-assign over a board you cannot see", which is the harm itself.
	if errors.Is(err, ErrBoardUnreadable) {
		httputil.RespondError(w, r, err.Error(), http.StatusBadGateway, err)
		return
	}
	var refusal *Refusal
	if errors.As(err, &refusal) {
		httputil.RespondError(w, r, refusal.Msg, http.StatusUnprocessableEntity, err)
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
