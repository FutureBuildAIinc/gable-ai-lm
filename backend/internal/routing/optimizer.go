// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

package routing

import "math"

// avgSpeedMph is the assumed average road speed used to estimate duration from
// distance for the MVP (no real routing engine).
const avgSpeedMph = 35.0

// optimizeSequence orders stops to minimize total travel distance starting from
// the depot, using nearest-neighbor construction followed by 2-opt improvement.
// It is deterministic for a given input. Returns the ordered stops, total
// distance (mi) and estimated duration (min).
func optimizeSequence(depotLat, depotLng float64, stops []Stop) ([]Stop, float64, float64) {
	n := len(stops)
	if n == 0 {
		return stops, 0, 0
	}

	// Nearest-neighbor construction from the depot.
	visited := make([]bool, n)
	order := make([]int, 0, n)
	curLat, curLng := depotLat, depotLng
	for len(order) < n {
		best := -1
		bestDist := math.Inf(1)
		for i := 0; i < n; i++ {
			if visited[i] {
				continue
			}
			d := haversineMiles(curLat, curLng, stops[i].Lat, stops[i].Lng)
			if d < bestDist {
				bestDist = d
				best = i
			}
		}
		visited[best] = true
		order = append(order, best)
		curLat, curLng = stops[best].Lat, stops[best].Lng
	}

	order = twoOpt(depotLat, depotLng, stops, order)

	// Materialize the ordered stops and totals.
	ordered := make([]Stop, n)
	total := 0.0
	pLat, pLng := depotLat, depotLng
	for seq, idx := range order {
		s := stops[idx]
		s.Sequence = seq + 1
		ordered[seq] = s
		total += haversineMiles(pLat, pLng, s.Lat, s.Lng)
		pLat, pLng = s.Lat, s.Lng
	}

	duration := total / avgSpeedMph * 60.0
	return ordered, round2(total), round2(duration)
}

// twoOpt repeatedly reverses route segments while doing so shortens the path.
// Deterministic: scans index pairs in order until no improvement is found.
func twoOpt(depotLat, depotLng float64, stops []Stop, order []int) []int {
	n := len(order)
	if n < 4 {
		return order
	}
	improved := true
	for improved {
		improved = false
		for i := 0; i < n-1; i++ {
			for k := i + 1; k < n; k++ {
				if delta := twoOptDelta(depotLat, depotLng, stops, order, i, k); delta < -1e-9 {
					reverse(order, i, k)
					improved = true
				}
			}
		}
	}
	return order
}

// twoOptDelta returns the change in total distance from reversing order[i..k].
func twoOptDelta(depotLat, depotLng float64, stops []Stop, order []int, i, k int) float64 {
	aLat, aLng := nodeAt(depotLat, depotLng, stops, order, i-1)
	bLat, bLng := stops[order[i]].Lat, stops[order[i]].Lng
	cLat, cLng := stops[order[k]].Lat, stops[order[k]].Lng

	// Edge after k (may be the implicit end; treat end as no return-to-depot).
	before := haversineMiles(aLat, aLng, bLat, bLng)
	var after float64
	if k+1 < len(order) {
		dLat, dLng := stops[order[k+1]].Lat, stops[order[k+1]].Lng
		before += haversineMiles(cLat, cLng, dLat, dLng)
		after = haversineMiles(aLat, aLng, cLat, cLng) + haversineMiles(bLat, bLng, dLat, dLng)
	} else {
		after = haversineMiles(aLat, aLng, cLat, cLng)
	}
	return after - before
}

// nodeAt returns coordinates of the node at position pos, or the depot for pos < 0.
func nodeAt(depotLat, depotLng float64, stops []Stop, order []int, pos int) (float64, float64) {
	if pos < 0 {
		return depotLat, depotLng
	}
	s := stops[order[pos]]
	return s.Lat, s.Lng
}

func reverse(order []int, i, k int) {
	for i < k {
		order[i], order[k] = order[k], order[i]
		i++
		k--
	}
}

func haversineMiles(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusMi = 3958.8
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLng := (lng2 - lng1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusMi * c
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
