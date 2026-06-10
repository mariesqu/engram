package controlapi

import (
	"encoding/json"
	"net/http"
	"time"
)

// handleConfigPut handles PUT /api/v1/config.
//
// Accepts a partial merge-patch JSON body (RFC 7396 semantics). Fields not
// present in the body are left unchanged. The handler rejects any body that
// contains writer_key or central_url — those are managed exclusively by the
// connect/disconnect endpoints.
//
// Response: {"restart_required": bool}
//
// The route is registered with WithAuthAndOrigin so bearer-token auth and
// Origin validation are enforced before this handler runs.
func (s *Server) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	// Decode into a raw map first to detect forbidden fields before applying
	// the typed patch. This is the RFC-7396 "partial merge" shape — we must
	// reject unknown forbidden keys without silently ignoring them.
	var raw map[string]json.RawMessage
	if !decodeBody(w, r, &raw) {
		return
	}

	// Reject forbidden fields: writer_key and central_url are managed by the
	// connect/disconnect endpoints and must never be changed here.
	forbidden := []string{"writer_key", "central_url", "writerKey", "centralUrl"}
	for _, f := range forbidden {
		if _, ok := raw[f]; ok {
			writeError(w, http.StatusBadRequest,
				"field "+f+" cannot be set via PUT /api/v1/config; use POST /api/v1/central/connect or /disconnect")
			return
		}
	}

	// Reject UNKNOWN keys with 400 rather than silently ignoring them — a
	// fat-fingered key name returning 200-no-op would mislead the caller.
	known := map[string]bool{
		"sync_interval": true, "log_level": true, "http_port": true,
		"db_path": true, "transport": true,
	}
	for k := range raw {
		if !known[k] {
			writeError(w, http.StatusBadRequest, "unknown config key: "+k)
			return
		}
	}

	// Re-encode the filtered map as JSON and decode into a typed ConfigPatch.
	// This two-step decode lets us use the raw map for field presence checks
	// while still benefiting from the typed struct for the patch fields.
	filtered, err := json.Marshal(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var patch ConfigPatch
	if err := json.Unmarshal(filtered, &patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid patch fields: "+err.Error())
		return
	}

	// Validate sync_interval up front: config.Patch silently skips unparseable
	// durations, which would turn a typo into a 200 no-op.
	if patch.SyncInterval != nil {
		d, err := time.ParseDuration(*patch.SyncInterval)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid sync_interval: must be a Go duration (e.g. \"30s\")")
			return
		}
		if d <= 0 {
			writeError(w, http.StatusBadRequest, "invalid sync_interval: must be positive")
			return
		}
	}

	restartRequired, err := s.cfgStore.Apply(patch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{
		"restart_required": restartRequired,
	})
}

// handleSyncTrigger handles POST /api/v1/sync/trigger.
//
// If central is not connected it returns 409 Conflict. Otherwise it triggers
// an immediate sync cycle and returns 202 Accepted.
//
// The route is registered with WithAuthAndOrigin.
func (s *Server) handleSyncTrigger(w http.ResponseWriter, r *http.Request) {
	st := s.syncCtrl.Status()
	if !st.CentralConnected {
		writeError(w, http.StatusConflict, "central not configured")
		return
	}

	if err := s.syncCtrl.TriggerNow(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status": "sync triggered",
	})
}
