package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// AssumedThreshold is the controller's bytes-per-request refusal the
// derivation (and every number in PREREG.md) was made at. A commit whose
// controller has another value is not re-derived silently: the arithmetic and
// the acceptance that rests on it are reviewed by a person first.
const AssumedThreshold = 8192.0

// Controller is what the derivation reads of celeris's adaptive controller at
// one commit (internal/adaptive/controller.go and telemetry.go).
type Controller struct {
	Threshold float64 // largePayloadBytes in newController
}

// shapeRule is one property of the controller or its telemetry that the
// derivation assumes, with the text that proves it at the commit read.
type shapeRule struct {
	file    string // controller.go or telemetry.go
	what    string // what the derivation assumes
	pattern *regexp.Regexp
}

// The statements the derivation stands on. Matched on source text with
// whitespace collapsed, so a reflow does not trip them and a change of meaning
// does.
var shapeRules = []shapeRule{
	{"controller.go", "the controller refuses a tick whose bytes per request is AT OR ABOVE the threshold (>=, not >)",
		regexp.MustCompile(`largePayload := snap\.BytesPerReq >= c\.largePayloadBytes`)},
	{"controller.go", "the refusal gates the single-tick snap path",
		regexp.MustCompile(`case !largePayload && cpw >= c\.highWatermark:`)},
	{"controller.go", "the refusal gates the sustained path",
		regexp.MustCompile(`case !largePayload && cpw >= c\.upThreshold:`)},
	{"controller.go", "a tick is 1 s (the echo's bytes per tick, the floor in requests a second and the acceptance's switch-tick bound are all per tick)",
		regexp.MustCompile(`evalInterval:\s*1 \* time\.Second`)},
	{"controller.go", "the sustained path promotes from 24 conns per worker",
		regexp.MustCompile(`upThreshold:\s*24\.0,`)},
	{"controller.go", "the snap path promotes from 48 conns per worker",
		regexp.MustCompile(`highWatermark:\s*48\.0,`)},
	{"controller.go", "the sustained path needs 2 consecutive ticks (the acceptance's switch-tick bound)",
		regexp.MustCompile(`sustainTicks:\s*2,`)},
	{"telemetry.go", "the tick's bytes are the engine's read plus written bytes, differenced between ticks",
		regexp.MustCompile(`deltaBytes := satSub\(m\.BytesRead\+m\.BytesWritten, prev\.BytesRead\+prev\.BytesWritten\)`)},
	{"telemetry.go", "the tick's requests are the engine's request count, differenced between ticks",
		regexp.MustCompile(`deltaReqs := satSub\(m\.RequestCount, prev\.RequestCount\)`)},
	{"telemetry.go", "bytes per request is the byte delta over the request delta",
		regexp.MustCompile(`snap\.BytesPerReq = float64\(deltaBytes\) / float64\(deltaReqs\)`)},
}

var thresholdRe = regexp.MustCompile(`largePayloadBytes:\s*([0-9]+(?:\.[0-9]+)?),`)

func collapse(s string) string {
	// Whitespace only: a comment inside a statement is a change to look at.
	return strings.Join(strings.Fields(s), " ")
}

// ReadController reads the controller's facts from a celeris tree (the module
// root) and checks every assumption of the derivation. Any difference is an
// error that names the file, the assumption and what to do; nothing is
// guessed.
func ReadController(celerisDir string) (Controller, error) {
	dir := filepath.Join(celerisDir, "internal", "adaptive")
	texts := map[string]string{}
	for _, f := range []string{"controller.go", "telemetry.go"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return Controller{}, fmt.Errorf("the adaptive controller's source is not where the derivation reads it (%s): %w", filepath.Join(dir, f), err)
		}
		texts[f] = collapse(string(b))
	}
	var c Controller
	m := thresholdRe.FindAllStringSubmatch(texts["controller.go"], -1)
	if len(m) != 1 {
		return c, fmt.Errorf("controller.go: found %d `largePayloadBytes: N,` initialisers, the derivation reads exactly one; review the controller before re-pinning", len(m))
	}
	v, err := strconv.ParseFloat(m[0][1], 64)
	if err != nil {
		return c, fmt.Errorf("controller.go: largePayloadBytes %q: %w", m[0][1], err)
	}
	c.Threshold = v
	var problems []string
	if v != AssumedThreshold {
		problems = append(problems, fmt.Sprintf("controller.go: the bytes-per-request threshold is %v, the derivation and PREREG.md assume %v", v, AssumedThreshold))
	}
	for _, r := range shapeRules {
		if !r.pattern.MatchString(texts[r.file]) {
			problems = append(problems, fmt.Sprintf("%s: no longer says what the derivation assumes: %s (looked for /%s/)", r.file, r.what, r.pattern))
		}
	}
	if len(problems) > 0 {
		return c, &ShapeError{Problems: problems}
	}
	return c, nil
}

// ShapeError means the controller at the commit read differs from what the
// derivation assumes. The re-pin stops there and changes nothing.
type ShapeError struct{ Problems []string }

func (e *ShapeError) Error() string {
	return "the controller at this commit differs from what the promotion-floor derivation assumes; nothing was changed. " +
		"Re-derive the floor and the period by hand (PREREG.md), then change the assumptions in tools/raceshape/shape.go in review:\n  - " +
		strings.Join(e.Problems, "\n  - ")
}

// IsShapeError reports whether err is a ShapeError.
func IsShapeError(err error) bool {
	var s *ShapeError
	return errors.As(err, &s)
}
