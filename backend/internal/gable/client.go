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

// PushDeliveryRoute writes an approved route back to GableLBM. Idempotent
// upstream on (vehicle_id, scheduled_date).
func (c *Client) PushDeliveryRoute(ctx context.Context, route DeliveryRoute) error {
	return c.do(ctx, http.MethodPost, "/api/integration/delivery-routes", route, nil)
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
		return fmt.Errorf("gable %s %s: status %d: %s", method, path, resp.StatusCode, string(snippet))
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
