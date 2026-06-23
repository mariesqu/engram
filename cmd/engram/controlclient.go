package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/mariesqu/engram/internal/controlapi"
)

// ErrDaemonNotRunning is returned when no daemon.json exists or when a stale
// token cannot be refreshed (daemon has exited or is restarting).
var ErrDaemonNotRunning = errors.New("engram daemon is not running")

// ControlClient makes HTTP requests to the control API of a running daemon.
// The client reads daemon.json on construction and re-reads it once on 401
// to handle a daemon restart (token rotation).
type ControlClient struct {
	dir   string // directory containing daemon.json (same dir as the DB)
	port  int
	token string
	http  *http.Client
}

// NewControlClient constructs a ControlClient by reading daemon.json from dir.
// Returns ErrDaemonNotRunning when the file does not exist.
func NewControlClient(dir string) (*ControlClient, error) {
	d, err := controlapi.ReadDaemonJSON(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: no daemon.json in %s", ErrDaemonNotRunning, dir)
		}
		return nil, fmt.Errorf("read daemon.json: %w", err)
	}
	return &ControlClient{
		dir:   dir,
		port:  d.Port,
		token: d.Token,
		http: &http.Client{
			Timeout: 5 * time.Second,
		},
	}, nil
}

// url builds the full URL for the given API path.
func (c *ControlClient) url(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", c.port, path)
}

// Get issues an authenticated GET request to the control API and decodes the
// JSON response body into dst. On 401 it re-reads daemon.json once and retries
// before returning ErrDaemonNotRunning.
func (c *ControlClient) Get(path string, dst any) error {
	resp, err := c.do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		// Token may be stale (daemon restarted). Re-read daemon.json once.
		if refreshErr := c.refresh(); refreshErr != nil {
			return fmt.Errorf("%w (stale token; %v)", ErrDaemonNotRunning, refreshErr)
		}
		resp2, err2 := c.do(http.MethodGet, path, nil)
		if err2 != nil {
			return err2
		}
		defer resp2.Body.Close()
		if resp2.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("%w (token mismatch after re-read)", ErrDaemonNotRunning)
		}
		return decodeResponse(resp2, dst)
	}

	return decodeResponse(resp, dst)
}

// Post issues an authenticated POST request with a JSON body.
func (c *ControlClient) Post(path string, body any, dst any) error {
	return c.mutate(http.MethodPost, path, body, dst)
}

// Put issues an authenticated PUT request with a JSON body.
func (c *ControlClient) Put(path string, body any, dst any) error {
	return c.mutate(http.MethodPut, path, body, dst)
}

// Delete issues an authenticated DELETE request with no body.
func (c *ControlClient) Delete(path string) error {
	resp, err := c.do(http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		if refreshErr := c.refresh(); refreshErr != nil {
			return fmt.Errorf("%w (stale token; %v)", ErrDaemonNotRunning, refreshErr)
		}
		resp2, err2 := c.do(http.MethodDelete, path, nil)
		if err2 != nil {
			return err2
		}
		defer resp2.Body.Close()
		if resp2.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("%w (token mismatch after re-read)", ErrDaemonNotRunning)
		}
		return decodeResponse(resp2, nil)
	}

	return decodeResponse(resp, nil)
}

// mutate is the shared implementation for Post and Put.
func (c *ControlClient) mutate(method, path string, body any, dst any) error {
	resp, err := c.do(method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		if refreshErr := c.refresh(); refreshErr != nil {
			return fmt.Errorf("%w (stale token; %v)", ErrDaemonNotRunning, refreshErr)
		}
		resp2, err2 := c.do(method, path, body)
		if err2 != nil {
			return err2
		}
		defer resp2.Body.Close()
		if resp2.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("%w (token mismatch after re-read)", ErrDaemonNotRunning)
		}
		return decodeResponse(resp2, dst)
	}

	return decodeResponse(resp, dst)
}

// do executes a single HTTP request with the current token.
func (c *ControlClient) do(method, path string, body any) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.url(path), bodyReader)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", c.port))
	}
	return c.http.Do(req)
}

// refresh re-reads daemon.json and updates port and token.
func (c *ControlClient) refresh() error {
	d, err := controlapi.ReadDaemonJSON(c.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrDaemonNotRunning
		}
		return err
	}
	c.port = d.Port
	c.token = d.Token
	return nil
}

// decodeResponse decodes a successful JSON response body into dst.
// If dst is nil the body is discarded. Returns an error for non-2xx responses.
func decodeResponse(resp *http.Response, dst any) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Try to extract an error message from the JSON body.
		var errBody struct {
			Error string `json:"error"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = json.Unmarshal(b, &errBody)
		if errBody.Error != "" {
			return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, errBody.Error)
		}
		return fmt.Errorf("daemon returned %d", resp.StatusCode)
	}
	if dst == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

// daemonDir returns the directory where daemon.json lives, derived from the
// DB path. It mirrors the directory used by the daemon on startup.
func daemonDir(dbPath string) string {
	return filepath.Dir(dbPath)
}
