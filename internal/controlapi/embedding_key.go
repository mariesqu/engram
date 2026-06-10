package controlapi

import (
	"errors"
	"net/http"
)

// ErrNoSecretStore is the sentinel error from config.ErrNoSecretStore.
// Declared here to avoid importing the config package (port-based architecture).
// The production wiring returns this from SealEmbeddingKey when the platform
// does not support secret storage. Exported so test stubs can return the exact
// sentinel and the handler's errors.Is check matches.
var ErrNoSecretStore = errors.New("secret store not available on this platform; use ENGRAM_EMBEDDING_KEY env var")

// handleEmbeddingKeyPost handles POST /api/v1/embedding/key.
//
// Body: {"key": "<plaintext key string>"}
//
// The handler decodes the key, delegates sealing and persistence to the
// EmbeddingKeyStore port, and returns 200 on success. The plaintext key is
// NEVER echoed in any response body, log, or error message.
//
// Status codes:
//   - 200 OK — key sealed and stored.
//   - 400 — missing or empty key field.
//   - 422 Unprocessable Entity — platform does not support secret storage
//     (ErrNoSecretStore); use ENGRAM_EMBEDDING_KEY env var instead.
//   - 500 — other internal error.
//
// The route is registered with WithAuthAndOrigin so both bearer-token auth
// and Origin validation are enforced before this handler runs.
func (s *Server) handleEmbeddingKeyPost(w http.ResponseWriter, r *http.Request) {
	if s.keyStore == nil {
		writeError(w, http.StatusNotImplemented, "embedding key management not configured")
		return
	}

	var body struct {
		Key string `json:"key"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	if body.Key == "" {
		writeError(w, http.StatusBadRequest, "key field is required and must not be empty")
		return
	}

	if err := s.keyStore.SealEmbeddingKey([]byte(body.Key)); err != nil {
		if errors.Is(err, ErrNoSecretStore) {
			writeError(w, http.StatusUnprocessableEntity,
				"this platform does not support key storage; set ENGRAM_EMBEDDING_KEY env var instead")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status": "embedding key stored",
		// Honest contract: the live provider is constructed at startup — a key
		// stored now activates on the NEXT daemon start, not immediately.
		"note": "restart the daemon for the key to take effect",
	})
}

// handleEmbeddingKeyDelete handles DELETE /api/v1/embedding/key.
//
// Clears any stored encrypted embedding key from the config file. After this
// call the daemon falls back to ENGRAM_EMBEDDING_KEY env var (if set) or Noop.
//
// The route is registered with WithAuthAndOrigin.
func (s *Server) handleEmbeddingKeyDelete(w http.ResponseWriter, r *http.Request) {
	if s.keyStore == nil {
		writeError(w, http.StatusNotImplemented, "embedding key management not configured")
		return
	}

	if err := s.keyStore.ClearEmbeddingKey(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status": "embedding key cleared",
	})
}
