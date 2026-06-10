package controlapi

import (
	"errors"
	"log/slog"
	"net/http"
)

// Sentinel errors the SyncController.Reconnect implementation wraps so the
// connect handler can map failures to client-safe responses without echoing
// internal error detail (paths, syscall messages, hex-decode dumps) to the
// client. The full wrapped detail is logged server-side only.
var (
	// ErrInvalidWriterKey — the supplied writer_key is malformed (not hex /
	// wrong length). Maps to 422.
	ErrInvalidWriterKey = errors.New("invalid writer_key")
	// ErrCredentialValidation — the remote probe rejected the credentials or
	// the central URL is unreachable. Maps to 422.
	ErrCredentialValidation = errors.New("credential validation failed")
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
		// Full detail (wrapped paths/syscall messages) goes to the server log
		// ONLY — the response carries the client-safe sentinel text.
		slog.Default().Warn("central connect failed", "error", err)
		switch {
		case errors.Is(err, ErrInvalidWriterKey):
			writeError(w, http.StatusUnprocessableEntity, ErrInvalidWriterKey.Error())
		case errors.Is(err, ErrCredentialValidation):
			writeError(w, http.StatusUnprocessableEntity, ErrCredentialValidation.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal error")
		}
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
