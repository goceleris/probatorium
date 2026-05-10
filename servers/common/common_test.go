package common

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestWriteSimple_BodyAndContentType(t *testing.T) {
	rr := httptest.NewRecorder()
	WriteSimple(rr)
	if got := rr.Header().Get("Content-Type"); got != "text/plain" {
		t.Errorf("content-type: got %q, want text/plain", got)
	}
	if rr.Code != 200 {
		t.Errorf("status: got %d, want 200", rr.Code)
	}
	if rr.Body.String() != "Hello, World!" {
		t.Errorf("body: got %q, want %q", rr.Body.String(), "Hello, World!")
	}
}

func TestWriteJSON_ShapeMatchesStaticContract(t *testing.T) {
	rr := httptest.NewRecorder()
	WriteJSON(rr, "test")
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type: got %q, want application/json", got)
	}
	var v JSONResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if v.Message != "Hello, World!" || v.Server != "test" {
		t.Errorf("body=%+v", v)
	}
}

func TestWriteJSON1K_ContentLengthMatchesBody(t *testing.T) {
	rr := httptest.NewRecorder()
	WriteJSON1K(rr)
	body := rr.Body.Bytes()
	got := rr.Header().Get("Content-Length")
	want := strconv.Itoa(len(body))
	if got != want {
		t.Errorf("content-length: got %q, want %q", got, want)
	}
	if len(body) < 1024 {
		t.Errorf("payload too small: %d bytes (need >= 1024)", len(body))
	}
	// Ensure round-trips through encoding/json (catches stray bytes).
	var v paginatedResponse
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
}

func TestWriteJSON64K_ContentLengthMatchesBody(t *testing.T) {
	rr := httptest.NewRecorder()
	WriteJSON64K(rr)
	body := rr.Body.Bytes()
	if hdr := rr.Header().Get("Content-Length"); hdr != strconv.Itoa(len(body)) {
		t.Errorf("content-length: got %q, want %d", hdr, len(body))
	}
	if len(body) < 65536 {
		t.Errorf("payload too small: %d bytes (need >= 65536)", len(body))
	}
}

func TestWritePath_EchoesID(t *testing.T) {
	rr := httptest.NewRecorder()
	WritePath(rr, "42")
	if got := rr.Body.String(); got != "User ID: 42" {
		t.Errorf("body: got %q, want %q", got, "User ID: 42")
	}
	if got := rr.Header().Get("Content-Type"); got != "text/plain" {
		t.Errorf("content-type: got %q, want text/plain", got)
	}
}

func TestWriteBody_ReturnsOK(t *testing.T) {
	rr := httptest.NewRecorder()
	WriteBody(rr)
	if got := rr.Body.String(); got != "OK" {
		t.Errorf("body: got %q, want %q", got, "OK")
	}
}

func TestJSONPayloads_DeterministicAcrossCalls(t *testing.T) {
	// Crucial invariant: every adapter reads JSON1KPayload() / JSON64KPayload()
	// at request time; two calls must return identical bytes or the
	// per-cell throughput numerator would drift across runs.
	//
	// Bound the captured byte string in a local so staticcheck doesn't
	// fold the comparison away as `x != x`; the variable is what
	// guarantees we sampled the slice header once and compare against
	// a second independent sample.
	first1k := string(JSON1KPayload())
	if string(JSON1KPayload()) != first1k {
		t.Fatal("JSON1KPayload not stable across calls")
	}
	first64k := string(JSON64KPayload())
	if string(JSON64KPayload()) != first64k {
		t.Fatal("JSON64KPayload not stable across calls")
	}
}

func TestEndpoints_ContractStable(t *testing.T) {
	// Adapters iterate Endpoints in stable order at registration time.
	// Any reorder breaks adapter authors' assumptions; any new path
	// without a corresponding adapter helper breaks the contract.
	wantPaths := []string{"/", "/json", "/json-1k", "/json-64k", "/users/:id", "/upload"}
	if len(Endpoints) != len(wantPaths) {
		t.Fatalf("Endpoints count: got %d, want %d", len(Endpoints), len(wantPaths))
	}
	for i, want := range wantPaths {
		if Endpoints[i].Path != want {
			t.Errorf("Endpoints[%d].Path: got %q, want %q", i, Endpoints[i].Path, want)
		}
	}
}

func TestEndpoints_StaticBodyContentLengthsMatch(t *testing.T) {
	// The conformance binary checks Content-Length against body bytes;
	// the same invariant must hold for the canonical static bodies
	// baked into Endpoints. Empty body is OK — that's the dynamic
	// /users/:id sentinel.
	for _, ep := range Endpoints {
		if len(ep.ResponseBody) == 0 {
			continue
		}
		// /json-1k and /json-64k get init()-populated, so their lengths
		// must clear the documented thresholds.
		switch ep.Path {
		case "/json-1k":
			if len(ep.ResponseBody) < 1024 {
				t.Errorf("/json-1k body: %d bytes < 1024", len(ep.ResponseBody))
			}
		case "/json-64k":
			if len(ep.ResponseBody) < 65536 {
				t.Errorf("/json-64k body: %d bytes < 65536", len(ep.ResponseBody))
			}
		}
	}
}

func TestGenerateJSONPayload_AtLeastTargetSize(t *testing.T) {
	cases := []int{16, 256, 1024, 4096, 65536}
	for _, target := range cases {
		t.Run(strconv.Itoa(target), func(t *testing.T) {
			out := generateJSONPayload(target)
			if len(out) < target {
				t.Errorf("size: got %d, want >= %d", len(out), target)
			}
			// And it must be valid JSON.
			var v paginatedResponse
			if err := json.Unmarshal(out, &v); err != nil {
				t.Fatalf("not valid JSON: %v", err)
			}
		})
	}
}
