// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

// Package gable is the HTTP client for GableLBM's /api/integration/* surface.
// AI_LM is a standalone service: it pulls its source-of-truth data (vehicles,
// orders, products+weight, branch locations) from GableLBM and writes approved
// routes back, all authenticated with the X-Integration-Key header.
package gable

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to a GableLBM instance over the integration API.
type Client struct {
	baseURL        string
	integrationKey string
	http           *http.Client
}

// NewClient builds a GableLBM integration client. baseURL is e.g.
// "http://localhost:8080"; integrationKey is sent as X-Integration-Key.
//
// A trailing slash on baseURL is stripped, because GABLE_API_URL is deployment
// configuration a human types and one keystroke there used to break write-back
// alone. Every path below starts with "/", so "http://host:8080/" produced
// "//api/integration/…". Go's http.ServeMux — which is what GableLBM serves
// with — cleans that path and answers 301 to the single-slash form, and Go's
// http.Client turns a 301 into a GET and drops the request body. The GETs
// (vehicles, orders, products) therefore kept working perfectly, while
// PushDeliveryRoute arrived as a bodyless GET on a POST-only route: the Load
// Builder looked healthy and only the one call that writes to the dispatch
// board failed, with a message naming a POST the server never saw.
func NewClient(baseURL, integrationKey string) *Client {
	return &Client{
		baseURL:        strings.TrimRight(baseURL, "/"),
		integrationKey: integrationKey,
		http:           &http.Client{Timeout: 15 * time.Second},
	}
}

// --- Wire types (mirror GableLBM integration responses) ---

// Vehicle is a fleet vehicle from GableLBM. Capacity is nullable upstream.
type Vehicle struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	VehicleType       string `json:"vehicle_type"`
	LicensePlate      string `json:"license_plate,omitempty"`
	CapacityWeightLbs *int   `json:"capacity_weight_lbs,omitempty"`
	Make              string `json:"make,omitempty"`
	Model             string `json:"model,omitempty"`
	Year              int    `json:"year,omitempty"`
}

// Driver is a fleet driver from GableLBM. A valid driver id is required on
// delivery-route write-back.
type Driver struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // ACTIVE/INACTIVE/ON_LEAVE
}

// Product is a catalog product including its per-unit weight and the PIM's
// canonical parametric geometry. The L/W/H fields are pointers so a nil value
// ("PIM has no geometry yet") is distinguishable from a real zero dimension;
// AI_LM falls back to its own override/default when they are nil.
type Product struct {
	ID             string   `json:"id"`
	SKU            string   `json:"sku"`
	Name           string   `json:"name"`
	Category       string   `json:"category,omitempty"`
	UOM            string   `json:"uom,omitempty"`
	WeightLbs      float64  `json:"weight_lbs"`
	LengthIn       *float64 `json:"length_in"`
	WidthIn        *float64 `json:"width_in"`
	HeightIn       *float64 `json:"height_in"`
	Stackable      *bool    `json:"stackable"`
	GeometrySource string   `json:"geometry_source,omitempty"`
}

// OrderLine is a single line item on an order.
type OrderLine struct {
	ProductID string  `json:"product_id"`
	SKU       string  `json:"sku"`
	Quantity  float64 `json:"quantity"`
	WeightLbs float64 `json:"weight_lbs"`
}

// Order is a confirmed order with optional delivery geolocation.
//
// BranchID is the yard the load ships from (orders.branch_id, NOT NULL in
// GableLBM since migration 062). It is a plain string, deliberately not a
// pointer and not omitempty: an empty value on the wire is an upstream defect
// AI_LM should be able to observe, not a legitimate "this order has no branch".
type Order struct {
	ID           string      `json:"id"`
	Status       string      `json:"status"`
	BranchID     string      `json:"branch_id"`
	CustomerName string      `json:"customer_name,omitempty"`
	Address      string      `json:"address,omitempty"`
	Latitude     *float64    `json:"latitude,omitempty"`
	Longitude    *float64    `json:"longitude,omitempty"`
	Lines        []OrderLine `json:"lines"`
}

// Location is one of the dealer's branches (yards) in GableLBM, matched against
// Order.BranchID so a run roots at the yard its load actually leaves from
// rather than at one globally configured depot.
//
// Latitude/Longitude are pointers for the same reason Product's geometry is:
// locations.latitude/longitude are backfilled lazily by geocoding the branch
// address, so nil means "this yard has never been geocoded" and must not
// collapse into a real 0,0 off the coast of Africa. A nil coordinate is a
// reason to fall back down the depot chain, never a place to root a route.
//
// Only active top-level BRANCH locations are returned, so an order whose
// branch_id has no match here is "branch unknown" — also a fallback, not a
// crash.
type Location struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Address   string   `json:"address,omitempty"`
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`
}

// RouteStop is a single stop in an approved delivery route written back to LBM.
type RouteStop struct {
	OrderID  string  `json:"order_id"`
	Sequence int     `json:"sequence"`
	Lat      float64 `json:"lat"`
	Lng      float64 `json:"lng"`
}

// DeliveryRoute is the write-back payload for an approved plan. LoadManifest
// carries the 3D packing manifest (pack steps per placement) that powers
// GableLBM's yard "Pack Trucks" instructions; any JSON-marshalable value works.
type DeliveryRoute struct {
	VehicleID     string      `json:"vehicle_id"`
	DriverID      string      `json:"driver_id,omitempty"`
	ScheduledDate string      `json:"scheduled_date"` // YYYY-MM-DD
	Stops         []RouteStop `json:"stops"`
	LoadManifest  any         `json:"load_manifest,omitempty"`
}

// RouteRecall withdraws a route AI_LM previously pushed from GableLBM's
// dispatch board. It is the inverse of DeliveryRoute and the reason a dispatch
// board can no longer outlive the plan that created it.
//
// It is keyed on (VehicleID, ScheduledDate) and NEVER on a stored route id.
// The push upstream is create-or-replace: every re-push mints a fresh
// delivery_routes row, so an id captured at push time either names nothing by
// the time a recall matters or — worse — names a route that now belongs to a
// different plan.
//
// Reason and RecalledBy carry the provenance of the manual approval that
// authorized the withdrawal (the workflow's 423 override). Both are optional on
// the wire so a recall is never blocked on missing attribution.
//
// The field order and tags here are pinned by GableLBM's contract suite
// (internal/integrations/testdata/ailm_client_contract.json, type "RouteRecall").
// Changing them breaks the integration in a way only that suite will catch.
type RouteRecall struct {
	VehicleID     string `json:"vehicle_id"`
	ScheduledDate string `json:"scheduled_date"` // YYYY-MM-DD
	Reason        string `json:"reason,omitempty"`
	RecalledBy    string `json:"recalled_by,omitempty"`
}

// RouteRecallResult acknowledges a withdrawal.
//
// Recalled is false when there was nothing on the board for that (vehicle,
// date) pair. That is a SUCCESS: a recall names a desired end state — "no live
// route for this truck on this day" — and the state already holds. It matters
// because this call exists to clean up after a partial push, so it WILL be
// retried; a retry of an already-successful recall that read as a failure would
// leave the caller unable to tell "converged" from "broken".
type RouteRecallResult struct {
	Recalled  bool   `json:"recalled"`
	RouteID   string `json:"route_id,omitempty"` // the route withdrawn; empty when Recalled is false
	StopCount int    `json:"stop_count"`
	Reason    string `json:"reason,omitempty"`
}

// BoardRoute is one delivery route as GableLBM's dispatch board actually holds
// it for a date.
//
//	GET /api/integration/delivery-routes?date=YYYY-MM-DD
//
// This is the SYSTEM OF RECORD, and it is why this type exists at all. Every
// other wire type here is data AI_LM consumes to make a plan; this one is the
// answer to "what does the dealer's board actually hold right now", which AI_LM
// previously could not ask. It could WRITE the board (PushDeliveryRoute) and
// UNDO a write (RecallDeliveryRoute) and never READ it, so every gate on this
// service keyed on AI_LM's own ledger — a cache of this board that diverges
// from it on any crash between the ERP write and the ledger write, with nothing
// able to notice.
//
// Status separates DRAFT and SCHEDULED (live, still recallable, still ours to
// re-plan) from IN_TRANSIT and COMPLETED (the truck has left the yard —
// history, and never reclaimable: GableLBM answers the recall with a 409). It
// is not decoration; without it every row on the board looks alike and a caller
// cannot tell a route it may withdraw from one a driver is currently driving.
//
// CANCELLED rows are returned deliberately, with RecalledAt set when the
// withdrawal came from us. That is how "my recall landed" is told apart from
// "the route never existed" — a distinction our ledger cannot make after a
// crash, and the exact blind spot this endpoint removes.
//
// VehicleID may be empty: delivery_routes.vehicle_id is nullable upstream and a
// route naming no truck is therefore NOT addressable by the
// (vehicle_id, scheduled_date) recall key. OrderIDs is always an array, never
// null; empty means the route currently puts nothing on the board.
//
// The field order and tags here are pinned by GableLBM's contract suite
// (internal/integrations/testdata/ailm_client_contract.json, type "BoardRoute").
// Changing them breaks the integration in a way only that suite will catch.
type BoardRoute struct {
	RouteID       string   `json:"route_id"`
	VehicleID     string   `json:"vehicle_id"`
	DriverID      string   `json:"driver_id,omitempty"`
	Status        string   `json:"status"`
	ScheduledDate string   `json:"scheduled_date"`
	StopCount     int      `json:"stop_count"`
	OrderIDs      []string `json:"order_ids"`
	RecalledAt    string   `json:"recalled_at,omitempty"`
}

// Board route statuses, as GableLBM's delivery_routes.status holds them.
//
// They are named here rather than spelled as literals at each comparison
// because the LIVE/DISPATCHED split below is a product decision — "may this
// service withdraw this route?" — and a decision spelled as a bare string in
// three files is a decision that drifts.
const (
	RouteStatusDraft     = "DRAFT"
	RouteStatusScheduled = "SCHEDULED"
	RouteStatusInTransit = "IN_TRANSIT"
	RouteStatusCompleted = "COMPLETED"
	RouteStatusCancelled = "CANCELLED"
)

// Live reports whether this route is on the board AND still withdrawable by
// this service.
//
// Only DRAFT and SCHEDULED qualify. IN_TRANSIT and COMPLETED are a truck that
// has left the yard: treating one as reclaimable means proposing to re-plan a
// run that is physically happening, and GableLBM refuses the recall with a 409
// (ErrRouteDispatched) precisely so that mistake cannot be made quietly.
// CANCELLED is not on the board at all.
func (r BoardRoute) Live() bool {
	return r.Status == RouteStatusDraft || r.Status == RouteStatusScheduled
}

// Dispatched reports that this route has left the yard. It is not live and it
// is not gone: it is history that no recall can reach, and a caller must be
// able to say so in a sentence rather than infer it from Live() being false —
// which is also true of a CANCELLED route, for the opposite reason.
func (r BoardRoute) Dispatched() bool {
	return r.Status == RouteStatusInTransit || r.Status == RouteStatusCompleted
}

// StaffValidation is the GableLBM /api/integration/validate-staff response. It
// reports whether a staff member's email is entitled to use AI_LM and carries
// the role/module grants that authorize the AI_LM session.
type StaffValidation struct {
	StaffID  string   `json:"staff_id"`
	Email    string   `json:"email"`
	Name     string   `json:"name"`
	Entitled bool     `json:"entitled"`
	Roles    []string `json:"roles"`
	Modules  []string `json:"modules"`
}

// APIError is a non-2xx answer from GableLBM, carrying the status code the
// caller needs in order to act on it. Before it existed, do() collapsed every
// upstream refusal into a formatted string, so a caller could not tell a 404
// from a 500 without parsing English — and the one caller that must (route
// recall, which treats "already gone" as success) could not be written at all.
//
// Error() reproduces that original string byte for byte. The message is the
// operator-facing contract pinned by TestErrorCarriesUpstreamStatusAndSnippet;
// this type adds a machine-facing one beside it without changing it.
type APIError struct {
	Status int
	Method string
	Path   string
	Body   string // up to 512 bytes of the upstream response
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gable %s %s: status %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// ErrRouteDispatched reports that a route could not be recalled because the
// truck has already left the yard (GableLBM answers 409 for an IN_TRANSIT or
// COMPLETED route). It is a distinct sentinel because the correct response is
// different in kind from every other failure here: not "retry", but "this
// cannot be undone from a screen — call the driver".
var ErrRouteDispatched = errors.New("route already dispatched; cannot recall")

// --- Methods ---

// ListVehicles returns the GableLBM fleet. GableLBM's integration endpoints
// return bare JSON arrays (not an enveloped object).
func (c *Client) ListVehicles(ctx context.Context) ([]Vehicle, error) {
	var out []Vehicle
	if err := c.do(ctx, http.MethodGet, "/api/integration/vehicles", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListLocations returns the dealer's active branches (yards). GableLBM's
// integration endpoints return bare JSON arrays (not an enveloped object).
func (c *Client) ListLocations(ctx context.Context) ([]Location, error) {
	var out []Location
	if err := c.do(ctx, http.MethodGet, "/api/integration/locations", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListDrivers returns the GableLBM drivers. GableLBM's integration endpoints
// return bare JSON arrays (not an enveloped object).
func (c *Client) ListDrivers(ctx context.Context) ([]Driver, error) {
	var out []Driver
	if err := c.do(ctx, http.MethodGet, "/api/integration/drivers", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetProductsWithWeight returns the catalog with per-unit weights.
func (c *Client) GetProductsWithWeight(ctx context.Context) ([]Product, error) {
	var out []Product
	if err := c.do(ctx, http.MethodGet, "/api/integration/products", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListOrdersForDate returns confirmed orders for a scheduled date (YYYY-MM-DD).
func (c *Client) ListOrdersForDate(ctx context.Context, date string) ([]Order, error) {
	q := url.Values{}
	q.Set("date", date)
	q.Set("status", "CONFIRMED")
	var out []Order
	if err := c.do(ctx, http.MethodGet, "/api/integration/orders?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListDeliveryRoutesForDate returns every delivery route GableLBM's dispatch
// board holds for a date — the system of record for who owns which truck that
// day, in the same bare-array envelope as every other integration GET.
//
// date is REQUIRED and GableLBM answers 400 without it, unlike
// ListOrdersForDate where it is optional. A dateless call is not a smaller
// question but a meaningless one: the seam's whole idempotency key is
// (vehicle_id, scheduled_date), and dropping the filter would stream every
// route the dealer has ever had.
//
// The result is UNFILTERED by status on purpose — cancelled and completed
// routes arrive alongside live ones. Filtering is the caller's job and is
// cheap; hiding rows here would be lossy, and one of the rows a naive filter
// would hide (CANCELLED with RecalledAt) is the one that answers "did my recall
// land?".
func (c *Client) ListDeliveryRoutesForDate(ctx context.Context, date string) ([]BoardRoute, error) {
	q := url.Values{}
	q.Set("date", date)
	var out []BoardRoute
	if err := c.do(ctx, http.MethodGet, "/api/integration/delivery-routes?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// PushDeliveryRoute writes an approved route back to GableLBM. Idempotent
// upstream on (vehicle_id, scheduled_date).
func (c *Client) PushDeliveryRoute(ctx context.Context, route DeliveryRoute) error {
	return c.do(ctx, http.MethodPost, "/api/integration/delivery-routes", route, nil)
}

// RecallDeliveryRoute withdraws a previously pushed route from GableLBM's
// dispatch board and its yard Pack-Trucks surface.
//
//	POST /api/integration/delivery-routes/recall
//
// Upstream supersedes the route (CANCELLED plus recall audit columns) rather
// than deleting it, so the audit trail and any proof-of-delivery attached to
// that day survive.
//
// Two response codes need naming because callers must branch on them:
//
//   - 200 with {"recalled": false} means there was nothing on the board. That
//     is success. Recall is idempotent by design so that a retry after a failed
//     multi-truck recall converges instead of reporting a phantom failure.
//   - 409 means the truck is already IN_TRANSIT or COMPLETED. That is
//     terminal, not retryable, and is returned as ErrRouteDispatched so a
//     caller can escalate it to a human instead of looping.
func (c *Client) RecallDeliveryRoute(ctx context.Context, recall RouteRecall) (*RouteRecallResult, error) {
	var out RouteRecallResult
	if err := c.do(ctx, http.MethodPost, "/api/integration/delivery-routes/recall", recall, &out); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
			return nil, fmt.Errorf("%w (truck %s on %s)", ErrRouteDispatched, recall.VehicleID, recall.ScheduledDate)
		}
		return nil, err
	}
	return &out, nil
}

// ValidateStaff asks GableLBM whether the given staff email is entitled to use
// AI_LM, returning the staff identity plus role/module grants. Sent with the
// X-Integration-Key header like every other integration call.
func (c *Client) ValidateStaff(ctx context.Context, email string) (*StaffValidation, error) {
	body := map[string]string{"email": email}
	var out StaffValidation
	if err := c.do(ctx, http.MethodPost, "/api/integration/validate-staff", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// do performs a JSON request against the integration API. body may be nil; out
// may be nil when no response decoding is needed.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-Integration-Key", c.integrationKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &APIError{Status: resp.StatusCode, Method: method, Path: path, Body: string(snippet)}
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			// Name the call, like the transport and status paths above do. A
			// bare "decode response: EOF" told an operator that SOMETHING
			// upstream answered in a shape AI_LM could not read, and nothing
			// about which of the six routes it was.
			return fmt.Errorf("decode %s %s response: %w", method, path, err)
		}
	}
	return nil
}
