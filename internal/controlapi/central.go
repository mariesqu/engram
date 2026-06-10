package controlapi

import (
	"net/http"
)

// ConnectRequest is the JSON body for POST /api/v1/central/connect.
type ConnectRequest struct {
	CentralURL string `json:"central_url"`
	WriterID   string `json:"writer_id"`
	WriterKey  string `json:"writer_key"`
}

// handleConnect handles POST /api/v1/central/connect.
//
// It validates the request, attempts to establish connectivity via
// SyncController.Reconnect, persists the config (via ConfigStore.Apply), and
// returns the updated status.
//
// On credential failure it returns 422 Unprocessable Entity and does NOT
// persist anything.
//
// The route is registered with WithAuthAndOrigin.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	var req ConnectRequest
	if !decodeBody(w, r, &req) {
		return
	}

	if req.CentralURL == "" {
		writeError(w, http.StatusBadRequest, "central_url is required")
		return
	}
	if req.WriterKey == "" {
		writeError(w, http.StatusBadRequest, "writer_key is required")
		return
	}

	// Attempt to connect and persist. The SyncController.Reconnect
	// implementation is responsible for:
	//   1. Validating credentials against the remote (probe).
	//   2. Sealing the writer key via DPAPI (on Windows).
	//   3. Persisting the config via cfgStore.Apply.
	//   4. Starting/rewiring the sync loop.
	//   5. Re-installing the SetCentralConfiguredFn closure.
	//
	// On failure (422) Reconnect must NOT persist any state change.
	cfg := CentralConfig{
		URL:      req.CentralURL,
		WriterID: req.WriterID,
	}
	// Pass the raw writer key as WriterKeyPlaintext — the adapter seals it.
	cfg.WriterKeyPlaintext = req.WriterKey

	if err := s.syncCtrl.Reconnect(cfg); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// Return the updated status after a successful connect.
	st := s.syncCtrl.Status()
	st.DaemonVersion = s.version
	writeJSON(w, http.StatusOK, st)
}

// handleDisconnect handles POST /api/v1/central/disconnect.
//
// Stops the sync loop, clears central credentials from the config file, and
// returns 200. Local data is NOT deleted.
//
// The route is registered with WithAuthAndOrigin.
func (s *Server) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.syncCtrl.Disconnect(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	st := s.syncCtrl.Status()
	st.DaemonVersion = s.version
	writeJSON(w, http.StatusOK, st)
}
