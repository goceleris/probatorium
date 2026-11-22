package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// refusalsMD is the frozen refusal list; its sha256 lands in COMPOSITION.json.
//
//go:embed REFUSALS.md
var refusalsMD []byte

func refusalsSHA256() string {
	h := sha256.Sum256(refusalsMD)
	return hex.EncodeToString(h[:])
}

// Refusal is one violated item of REFUSALS.md.
type Refusal struct {
	ID  string // "R1".."R22"
	Msg string
}

// RefusalError carries every violated item.
type RefusalError struct{ Items []Refusal }

func (e *RefusalError) Error() string {
	var sb strings.Builder
	for i, r := range e.Items {
		if i > 0 {
			sb.WriteByte('\n')
		}
		fmt.Fprintf(&sb, "REFUSE %s: %s", r.ID, r.Msg)
	}
	return sb.String()
}

// IDs returns the sorted unique IDs of the violated items.
func (e *RefusalError) IDs() []string {
	seen := map[string]bool{}
	var ids []string
	for _, r := range e.Items {
		if !seen[r.ID] {
			seen[r.ID] = true
			ids = append(ids, r.ID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return idNum(ids[i]) < idNum(ids[j]) })
	return ids
}

func idNum(id string) int {
	n := 0
	fmt.Sscanf(strings.TrimPrefix(id, "R"), "%d", &n)
	return n
}

// refusals accumulates violations during a composition.
type refusals struct{ items []Refusal }

func (r *refusals) add(id, format string, a ...any) {
	r.items = append(r.items, Refusal{ID: id, Msg: fmt.Sprintf(format, a...)})
}

func (r *refusals) err() error {
	if len(r.items) == 0 {
		return nil
	}
	return &RefusalError{Items: r.items}
}

// need is the R19 rule: a value missing on either side is a refusal. It
// reports whether the value is present.
func (r *refusals) need(side, what, v string) bool {
	if strings.TrimSpace(v) == "" {
		r.add("R19", "%s: %s is missing, so equality with the other document cannot be established", side, what)
		return false
	}
	return true
}
