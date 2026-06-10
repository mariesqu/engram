//go:build windows

package tray

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// newControlRequest builds an authenticated HTTP request for the control API.
// The bearer token and Origin header are set per the control API protocol.
func newControlRequest(method, url, token string, port int, body any) (*http.Request, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", port))
	}
	return req, nil
}

// decodeJSON decodes a JSON response body into dst.
func decodeJSON(resp *http.Response, dst any) error {
	return json.NewDecoder(resp.Body).Decode(dst)
}

// execDetached launches a subprocess detached from the current process group.
// On Windows this uses CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS so the child
// process is fully independent and not tied to the tray's console or lifetime.
func execDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}
	return cmd.Start()
}
