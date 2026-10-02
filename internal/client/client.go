// Package client is the CLI side of the daemon API: HTTP over unix socket.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/alekzonder/tariboy/internal/version"
)

type Client struct {
	sock string
	http *http.Client
}

type APIError struct {
	Code string
	Msg  string
	// Details is the error's structured data, when the daemon sent any.
	Details map[string]any
}

func (e *APIError) Error() string { return e.Code + ": " + e.Msg }

func New(sock string) *Client {
	return &Client{
		sock: sock,
		http: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}},
	}
}

func (c *Client) Call(method, route string, body any) (json.RawMessage, error) {
	u := "http://unix" + route
	var reqBody *bytes.Reader
	if method == http.MethodGet || method == http.MethodDelete {
		if qp, ok := body.(map[string]string); ok && len(qp) > 0 {
			vals := url.Values{}
			for k, v := range qp {
				vals.Set(k, v)
			}
			u += "?" + vals.Encode()
		}
		reqBody = bytes.NewReader(nil)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		if body == nil {
			b = []byte("{}")
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	warnVersionMismatch(resp.Header.Get(version.Header))
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("bad response from daemon: %w", err)
	}
	if !env.OK {
		if env.Error == nil {
			return nil, errors.New("daemon returned failure without error detail")
		}
		return nil, &APIError{Code: env.Error.Code, Msg: env.Error.Message, Details: env.Error.Details}
	}
	return env.Result, nil
}

// Upload sends a raw archive body to an operator API route over the daemon's
// Unix socket and decodes the standard API envelope.
func (c *Client) Upload(route string, body io.Reader, size int64) (json.RawMessage, error) {
	return c.upload(http.MethodPost, route, "application/gzip", body, size)
}

func (c *Client) UploadFile(route string, body io.Reader, size int64) (json.RawMessage, error) {
	return c.upload(http.MethodPut, route, "application/octet-stream", body, size)
}

func (c *Client) upload(method, route, contentType string, body io.Reader, size int64) (json.RawMessage, error) {
	req, err := http.NewRequest(method, "http://unix"+route, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = size
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	warnVersionMismatch(resp.Header.Get(version.Header))
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("bad response from daemon: %w", err)
	}
	if !env.OK {
		if env.Error == nil {
			return nil, errors.New("daemon returned failure without error detail")
		}
		return nil, &APIError{Code: env.Error.Code, Msg: env.Error.Message}
	}
	return env.Result, nil
}

// Stream opens a Server-Sent Events request and calls onEvent per event until
// the connection closes or ctx-less error. Used by `logs -f` / `channel tail -f`.
func (c *Client) Stream(ctx context.Context, route string, onEvent func(eventType string, data []byte)) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://unix"+route, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var evType string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			evType = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			onEvent(evType, []byte(strings.TrimPrefix(line, "data: ")))
			evType = ""
		}
	}
	return sc.Err()
}

var (
	// versionWarned latches the mismatch warning for the life of the process: a
	// single agent iteration makes dozens of calls, and one line is a signal
	// while dozens are noise. Package state, not per-Client, so short-lived
	// clients created per command still warn only once; atomic because Call may
	// run from several goroutines.
	versionWarned  atomic.Bool
	versionWarnOut io.Writer = os.Stderr
)

// warnVersionMismatch prints one line to stderr when the daemon reports a build
// other than this binary's. An empty header means a daemon older than the
// header itself — nothing to compare, so nothing is said. The warning never
// changes stdout or the exit code: it is a diagnostic, and callers parse stdout.
func warnVersionMismatch(daemonVersion string) {
	if daemonVersion == "" || daemonVersion == version.Version {
		return
	}
	if !versionWarned.CompareAndSwap(false, true) {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "unknown"
	}
	fmt.Fprintf(versionWarnOut,
		"warning: client version %s does not match daemon version %s; this client (%s) may not know the daemon's newer flags\n",
		version.Version, daemonVersion, exe)
}

// IsDaemonDown reports errors that mean "tariboyd is not running here".
func IsDaemonDown(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENOENT) ||
		(err != nil && strings.Contains(err.Error(), "no such file"))
}
