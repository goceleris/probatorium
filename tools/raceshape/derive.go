package main

import (
	"fmt"
	"math"
	"time"
)

// Measured is what the derivation takes from the cluster run that showed the
// saturation (race validation run 39560945814, msa2-server, the adaptive cell
// of auth_session_ratelimit: its properties_series.csv and tier1_tally.json).
// None of it depends on the celeris commit, so a re-pin does not touch it; what
// a re-pin re-reads is the controller's side of the arithmetic (Controller).
// A new measurement is a decision made here, in review, with its own run.
type Measured struct {
	// EchoBytesPerTick is the most bytes a healthy 64 KiB echo adds to the
	// engine's read+written bytes in one 1 s controller tick without adding a
	// request (frames are not requests).
	EchoBytesPerTick float64
	// BaselineBytesPerReq is the bytes per request of the pure-engine race cells
	// (10th percentile, no echo).
	BaselineBytesPerReq float64
	// CellBytesPerReq is the bytes per request this cell itself ran at.
	CellBytesPerReq float64
	// CellExtraBytesPerTick is the cell's bytes per second that do not scale
	// with requests (25.7 MB/s observed less 717 B x 17.7k requests/s).
	CellExtraBytesPerTick float64
	// MarkovWalkers is the number of Markov walkers of the cell: 110 walkers
	// less 22 adversarial, 11 h2c, 5 ws and 5 sse.
	MarkovWalkers int
	// SaturatingRPS is the rate the Markov fleet sent closed-loop when the cell
	// saturated its two engine loops.
	SaturatingRPS float64
}

// Run39560945814 is the measurement the pinned numbers rest on.
var Run39560945814 = Measured{
	EchoBytesPerTick:      49.4e6,
	BaselineBytesPerReq:   717,
	CellBytesPerReq:       1452,
	CellExtraBytesPerTick: 13.0e6,
	MarkovWalkers:         67,
	SaturatingRPS:         16560,
}

// The rules that turn the floor into a period. They are decisions, not
// measurements (PREREG.md), so they live here and are not derived.
const (
	// FloorMargin: the fleet's ceiling must be at least this far above the
	// promotion floor.
	FloorMargin = 1.15
	// SaturatingFraction: the ceiling must stay at or under this share of the
	// rate that saturated the cell, or the pacing removes too little.
	SaturatingFraction = 0.7
	// FloorGranularity is the step the floor is rounded UP to, in requests a
	// second.
	FloorGranularity = 50
	// PeriodGrid is the step the period is chosen on, rounded DOWN from the
	// longest period the margin allows.
	PeriodGrid = 500 * time.Microsecond
)

// Scenario is one reading of the promotion floor.
type Scenario struct {
	Name        string
	BytesPerReq float64 // b0
	BytesTick   float64 // X
	FloorRPS    float64 // R > X / (threshold - b0)
}

// Result is the derivation at one celeris commit.
type Result struct {
	Threshold   float64 // the controller's bytes-per-request refusal, from the source
	Scenarios   []Scenario
	FloorRPS    float64 // the largest scenario, rounded up to FloorGranularity
	Period      time.Duration
	CeilingRPS  float64 // MarkovWalkers / Period
	MaxPeriod   time.Duration
	MinPeriod   time.Duration
	Measured    Measured
	CeilingOver float64 // ceiling / floor
	Saturating  float64 // ceiling / SaturatingRPS
}

// Derive computes the promotion floor and the Markov period from the
// controller's refusal threshold t and the measurement m.
//
// A tick is promotable when (b0 R + X) / R < t, with R the engine request rate
// and X the bytes per tick that do not scale with R, i.e. R > X / (t - b0). The
// floor is the largest of the three readings (pure-engine baseline, this
// cell's own ratio, baseline plus the cell's own fixed bytes), rounded up. The
// period is the longest one on the grid whose ceiling, MarkovWalkers / period,
// is still FloorMargin above the floor; it fails if that period leaves the
// other side of the window (the ceiling above SaturatingFraction of the rate
// that saturated).
func Derive(t float64, m Measured) (Result, error) {
	if m.MarkovWalkers <= 0 || m.SaturatingRPS <= 0 {
		return Result{}, fmt.Errorf("the measurement has no Markov walkers or saturating rate: %+v", m)
	}
	r := Result{Threshold: t, Measured: m}
	add := func(name string, b0, x float64) error {
		if t <= b0 {
			return fmt.Errorf("the controller's threshold %.0f B/request is not above the %s ratio %.0f B/request: no request rate can promote", t, name, b0)
		}
		r.Scenarios = append(r.Scenarios, Scenario{Name: name, BytesPerReq: b0, BytesTick: x, FloorRPS: x / (t - b0)})
		return nil
	}
	if err := add("A", m.BaselineBytesPerReq, m.EchoBytesPerTick); err != nil {
		return r, err
	}
	if err := add("B", m.CellBytesPerReq, m.EchoBytesPerTick); err != nil {
		return r, err
	}
	if err := add("C", m.BaselineBytesPerReq, m.EchoBytesPerTick+m.CellExtraBytesPerTick); err != nil {
		return r, err
	}
	largest := 0.0
	for _, s := range r.Scenarios {
		largest = math.Max(largest, s.FloorRPS)
	}
	r.FloorRPS = math.Ceil(largest/FloorGranularity) * FloorGranularity

	w := float64(m.MarkovWalkers)
	maxPeriod := time.Duration(w / (FloorMargin * r.FloorRPS) * float64(time.Second))
	r.MaxPeriod = maxPeriod
	r.MinPeriod = time.Duration(math.Ceil(w / (SaturatingFraction * m.SaturatingRPS) * float64(time.Second)))
	r.Period = maxPeriod / PeriodGrid * PeriodGrid
	if r.Period <= 0 || r.Period < r.MinPeriod {
		return r, fmt.Errorf("no period on the %v grid fits: the floor %.0f requests/s allows at most %v, the saturating rate %.0f requests/s needs at least %v",
			PeriodGrid, r.FloorRPS, maxPeriod, m.SaturatingRPS, r.MinPeriod)
	}
	r.CeilingRPS = w / r.Period.Seconds()
	r.CeilingOver = r.CeilingRPS / r.FloorRPS
	r.Saturating = r.CeilingRPS / m.SaturatingRPS
	return r, nil
}
