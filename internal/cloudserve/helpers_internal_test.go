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
