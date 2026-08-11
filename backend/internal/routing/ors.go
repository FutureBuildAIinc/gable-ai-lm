// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package routing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"
)

// OpenRouteService-backed routing provider. It pulls a real road
// distance/duration matrix (driving-hgv where available) and sequences each
// truck's stops over it with the same nearest-neighbor + 2-opt construction
// used by the haversine heuristic. It degrades gracefully: an unset key, a
// trivial stop count, or any ORS error falls back to the haversine optimizer so
// the workflow never hard-fails.

const (
	defaultORSBaseURL = "https://api.openrouteservice.org"
	defaultORSProfile = "driving-hgv"
	orsMatrixCeiling  = 50 // ORS free-tier matrix cap (locations per request)
)

// ORSProvider implements SequenceOptimizer against OpenRouteService.
type ORSProvider struct {
	apiKey   string
	baseURL  string
	profile  string
	http     *http.Client
	fallback SequenceOptimizer
}

// NewORSProvider builds an ORS-backed optimizer. An empty apiKey yields a
// provider that always falls back to haversine (so it is safe to install
// unconditionally). baseURL/profile default to the ORS public API and
// driving-hgv when empty.
func NewORSProvider(apiKey, baseURL, profile string) *ORSProvider {
	if baseURL == "" {
		baseURL = defaultORSBaseURL
	}
	if profile == "" {
		profile = defaultORSProfile
	}
	return &ORSProvider{
		apiKey:   strings.TrimSpace(apiKey),
		baseURL:  strings.TrimRight(baseURL, "/"),
		profile:  profile,
		http:     &http.Client{Timeout: 15 * time.Second},
		fallback: haversineOptimizer{},
	}
}

func (p *ORSProvider) Name() string {
	if p.apiKey == "" {
		return "ors(fallback:haversine)"
	}
	return "ors(" + p.profile + ")"
}

// Configured reports whether a key is present (real ORS calls are possible).
func (p *ORSProvider) Configured() bool { return p != nil && p.apiKey != "" }

// Sequence orders the stops using the ORS distance matrix, falling back to the
// haversine heuristic when ORS is unavailable.
func (p *ORSProvider) Sequence(ctx context.Context, depotLat, depotLng float64, stops []Stop) ([]Stop, float64, float64) {
	n := len(stops)
	// No benefit (or no key, or too many locations) — use the haversine path.
	if p.apiKey == "" || n <= 1 || n+1 > orsMatrixCeiling {
		return p.fallback.Sequence(ctx, depotLat, depotLng, stops)
	}

	dist, dur, err := p.matrix(ctx, depotLat, depotLng, stops)
	if err != nil {
		slog.Warn("ORS matrix failed; falling back to haversine optimizer", "error", err, "stops", n)
		return p.fallback.Sequence(ctx, depotLat, depotLng, stops)
	}

	// Matrix index 0 is the depot; stop k lives at matrix index k+1.
	order := sequenceMatrix(n, dist)

	ordered := make([]Stop, n)
	var totalDist, totalDur float64
	prev := 0 // depot
	for seq, idx := range order {
		s := stops[idx]
		s.Sequence = seq + 1
		ordered[seq] = s
		totalDist += dist[prev][idx+1]
		totalDur += dur[prev][idx+1]
		prev = idx + 1
	}
	// ORS distances come back in miles (units=mi); durations in seconds.
	return ordered, round2(totalDist), round2(totalDur / 60.0)
}

// --- ORS matrix HTTP ----------------------------------------------------------

type orsMatrixRequest struct {
	Locations [][2]float64 `json:"locations"` // [lng, lat]
	Metrics   []string     `json:"metrics"`
	Units     string       `json:"units"`
}

// orsMatrixResponse decodes an ORS matrix reply. The cells are *float64 (not
// float64) because ORS emits JSON null for a pair it cannot route between — a
// bad geocode, an off-network stop, or a location unreachable under the
// driving-hgv profile. Decoding a null into a plain float64 silently yields
// 0.0 while preserving the row length, so the shape checks pass and the
// nearest-neighbor construction then treats that leg as the CHEAPEST possible
// edge: the unroutable stop is pulled to the front of the route and the totals
// (which feed the shift-feasibility check and the dispatcher's decision) are
// under-reported. A pointer makes "no route" distinguishable from "zero miles".
type orsMatrixResponse struct {
	Distances [][]*float64 `json:"distances"`
	Durations [][]*float64 `json:"durations"`
	Error     any          `json:"error,omitempty"`
}

// errUnroutablePair marks a matrix that contains at least one pair ORS could
// not route between. It is treated exactly like any other matrix failure:
// Sequence falls back to the haversine optimizer rather than sequencing over a
// corrupt cost matrix.
var errUnroutablePair = errors.New("unroutable location pair in ORS matrix")

// matrix requests an (n+1)x(n+1) distance (mi) + duration (s) matrix with the
// depot at index 0. ORS uses [lng, lat] ordering (reverse of lat/lng).
func (p *ORSProvider) matrix(ctx context.Context, depotLat, depotLng float64, stops []Stop) (dist [][]float64, dur [][]float64, err error) {
	locations := make([][2]float64, 0, len(stops)+1)
	locations = append(locations, [2]float64{depotLng, depotLat}) // [lng, lat]
	for _, s := range stops {
		locations = append(locations, [2]float64{s.Lng, s.Lat})
	}

	payload, err := json.Marshal(orsMatrixRequest{
		Locations: locations,
		Metrics:   []string{"distance", "duration"},
		Units:     "mi",
	})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal matrix request: %w", err)
	}

	url := p.baseURL + "/v2/matrix/" + p.profile
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, fmt.Errorf("build matrix request: %w", err)
	}
	// ORS POST endpoints take the raw API key in Authorization (no "Bearer").
	req.Header.Set("Authorization", p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("matrix request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("matrix status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var mr orsMatrixResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return nil, nil, fmt.Errorf("decode matrix response: %w", err)
	}
	dist, err = solidifyMatrix("distance", mr.Distances, locations)
	if err != nil {
		return nil, nil, err
	}
	dur, err = solidifyMatrix("duration", mr.Durations, locations)
	if err != nil {
		return nil, nil, err
	}
	return dist, dur, nil
}

// solidifyMatrix validates one ORS cost matrix and converts it to plain
// float64s. It enforces the exact (n+1)×(n+1) shape (rejecting a garbage
// response) and, critically, that every cell is a real, usable cost:
//
//   - null            ⇒ ORS could not route the pair (bad geocode / off-network)
//   - NaN / ±Inf      ⇒ not a usable cost
//   - negative        ⇒ nonsensical cost
//   - zero off-diagonal between two DIFFERENT coordinates ⇒ a phantom
//     free leg, which nearest-neighbor would greedily take first
//
// Two orders delivered to the same geocoded jobsite legitimately cost 0 to
// travel between, so an exact-zero cell is only rejected when the coordinates
// actually differ. Any violation is an error, which routes the caller into the
// well-tested haversine fallback instead of a plausible-but-wrong road route.
func solidifyMatrix(kind string, m [][]*float64, locations [][2]float64) ([][]float64, error) {
	want := len(locations)
	if len(m) != want {
		return nil, fmt.Errorf("%s matrix shape mismatch: got %d rows for %d locations", kind, len(m), want)
	}
	out := make([][]float64, want)
	for i := 0; i < want; i++ {
		if len(m[i]) != want {
			return nil, fmt.Errorf("%s matrix row %d malformed: %d cells for %d locations", kind, i, len(m[i]), want)
		}
		out[i] = make([]float64, want)
		for j := 0; j < want; j++ {
			cell := m[i][j]
			if cell == nil {
				return nil, fmt.Errorf("%s %d→%d: %w (ORS returned null)", kind, i, j, errUnroutablePair)
			}
			v := *cell
			switch {
			case math.IsNaN(v) || math.IsInf(v, 0):
				return nil, fmt.Errorf("%s %d→%d: %w (non-finite value)", kind, i, j, errUnroutablePair)
			case v < 0:
				return nil, fmt.Errorf("%s %d→%d: %w (negative value %v)", kind, i, j, errUnroutablePair, v)
			case v == 0 && i != j && locations[i] != locations[j]:
				return nil, fmt.Errorf("%s %d→%d: %w (zero cost between distinct coordinates)", kind, i, j, errUnroutablePair)
			}
			out[i][j] = v
		}
	}
	return out, nil
}

// --- nearest-neighbor + 2-opt over a precomputed matrix -----------------------

// sequenceMatrix orders n stops over an (n+1)×(n+1) cost matrix (index 0 is the
// depot; stop k is index k+1). It mirrors the haversine optimizer: NN
// construction from the depot, then open-path 2-opt (no return to depot).
func sequenceMatrix(n int, cost [][]float64) []int {
	visited := make([]bool, n)
	order := make([]int, 0, n)
	cur := 0 // depot matrix index
	for len(order) < n {
		best := -1
		bestDist := math.Inf(1)
		for j := 0; j < n; j++ {
			if visited[j] {
				continue
			}
			if d := cost[cur][j+1]; d < bestDist {
				bestDist = d
				best = j
			}
		}
		visited[best] = true
		order = append(order, best)
		cur = best + 1
	}

	if n < 4 {
		return order
	}
	improved := true
	for improved {
		improved = false
		for i := 0; i < n-1; i++ {
			for k := i + 1; k < n; k++ {
				if matrixTwoOptDelta(cost, order, i, k) < -1e-9 {
					reverse(order, i, k)
					improved = true
				}
			}
		}
	}
	return order
}

// matrixTwoOptDelta returns the change in open-path cost from reversing
// order[i..k]. matrixIdx maps a position to its matrix index (-1 ⇒ depot ⇒ 0).
func matrixTwoOptDelta(cost [][]float64, order []int, i, k int) float64 {
	a := matrixIdx(order, i-1)
	b := order[i] + 1
	c := order[k] + 1

	before := cost[a][b]
	var after float64
	if k+1 < len(order) {
		d := order[k+1] + 1
		before += cost[c][d]
		after = cost[a][c] + cost[b][d]
	} else {
		after = cost[a][c]
	}
	return after - before
}

// matrixIdx returns the matrix index of the node at position pos, or 0 (the
// depot) for pos < 0.
func matrixIdx(order []int, pos int) int {
	if pos < 0 {
		return 0
	}
	return order[pos] + 1
}
