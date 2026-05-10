package common

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// JSONResponse is the canonical body shape served on /json. Kept in
// sync with the static bytes baked into Endpoints[/json].ResponseBody —
// any change here MUST update that constant or framework-side responses
// drift apart.
type JSONResponse struct {
	Message string `json:"message"`
	Server  string `json:"server"`
}

// WriteSimple writes the canonical "Hello, World!" plain-text response
// (Endpoints[/].ResponseBody). Used by net/http-based adapters; the
// fasthttp-based ones build the response on their own ctx so they
// bypass this helper.
func WriteSimple(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Hello, World!"))
}

// WriteJSON writes the canonical /json body with the adapter's server
// name baked into the JSONResponse.Server field. Adapters that want a
// fully static body can write Endpoints[/json].ResponseBody instead;
// this variant is preserved for the legacy gin/echo/iris helpers.
func WriteJSON(w http.ResponseWriter, serverType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	data, _ := json.Marshal(JSONResponse{
		Message: "Hello, World!",
		Server:  serverType,
	})
	_, _ = w.Write(data)
}

// WriteJSON1K writes the pre-computed ~1 KiB JSON payload with a
// matching Content-Length so HTTP/1.1 keep-alive doesn't fall back to
// chunked encoding under load.
func WriteJSON1K(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(json1KPayload)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(json1KPayload)
}

// WriteJSON64K writes the pre-computed ~64 KiB JSON payload.
func WriteJSON64K(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(json64KPayload)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(json64KPayload)
}

// WritePath writes the /users/:id response with the path parameter
// echoed in the body.
func WritePath(w http.ResponseWriter, id string) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("User ID: " + id))
}

// WriteBody acknowledges a /upload POST. The handler is expected to
// have already drained r.Body before calling this — every adapter does
// so deliberately so the body parser is part of the measured cost.
func WriteBody(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}
