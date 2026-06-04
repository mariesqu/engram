package cloudserve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWriteJSON_MarshalFailure_StillJSON proves writeJSON honors the JSON
// {"error":...} contract even when the value fails to marshal — it must never fall
// back to a text/plain response. This path is unreachable through the public
// handlers (all response types are controlled and always marshal), so it is
// exercised here in-package by passing an unmarshalable value directly.
func TestWriteJSON_MarshalFailure_StillJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	// A channel cannot be JSON-marshaled, forcing the marshal-failure path.
	writeJSON(rec, http.StatusOK, make(chan int))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 on marshal failure", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("fallback body is not valid JSON: %v (body=%q)", err, rec.Body.String())
	}
	if body.Error == "" {
		t.Error("fallback body: error field is empty, want a message")
	}
}

// TestHTTPServer_HasTimeouts asserts Run's server is constructed with the hardened
// read/write/idle timeouts (Slowloris defense), so they can't be silently dropped
// in a future refactor.
func TestHTTPServer_HasTimeouts(t *testing.T) {
	srv := New(nil).httpServer(":0")

	if srv.ReadHeaderTimeout != readHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, readHeaderTimeout)
	}
	if srv.ReadTimeout != readTimeout {
		t.Errorf("ReadTimeout = %v, want %v", srv.ReadTimeout, readTimeout)
	}
	if srv.WriteTimeout != writeTimeout {
		t.Errorf("WriteTimeout = %v, want %v", srv.WriteTimeout, writeTimeout)
	}
	if srv.IdleTimeout != idleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", srv.IdleTimeout, idleTimeout)
	}
}
