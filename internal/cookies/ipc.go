package cookies

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// IPC between the plugin and the native host runs over TCP loopback
// (127.0.0.1:<ephemeral>) with the listener's port recorded in a
// sidecar file the plugin reads at dial time. This replaced an
// AF_UNIX implementation that, on Windows, would eventually park the
// plugin process in a state where every dial failed with WSAEINVAL
// ("an invalid argument was supplied") until the plugin restarted —
// a known Go/Windows AF_UNIX quirk. TCP loopback has no such state,
// is cross-platform, and stays in the stdlib.

// ipcPortPath returns the filesystem path of the sidecar file that
// holds the listener's port. It's a var so tests can swap in a temp
// path.
var ipcPortPath = defaultIPCPortPath

// defaultIPCPortPath returns the platform-appropriate sidecar port file
// location, or "" on platforms that don't support the IPC path.
func defaultIPCPortPath() string {
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = os.TempDir()
		}
		dir := filepath.Join(base, "UsageButtons")
		_ = os.MkdirAll(dir, 0o755)
		return filepath.Join(dir, "ipc.port")
	case "darwin":
		return fmt.Sprintf("/tmp/usagebuttons-%d.port", os.Getuid())
	default:
		return ""
	}
}

// IPCAddress reports the current TCP listener address ("127.0.0.1:<port>")
// by reading the port file the native host wrote. Returns "" when no
// listener is published. Used for diagnostic logging from the PI.
func IPCAddress() string {
	path := ipcPortPath()
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	port := strings.TrimSpace(string(data))
	if port == "" {
		return ""
	}
	return "127.0.0.1:" + port
}

// ListenIPC opens the listener the native host uses to serve the
// plugin. Binds 127.0.0.1 on an OS-assigned port and atomically
// publishes that port to ipcPortPath so dialers can discover it.
// The returned listener removes the port file on Close.
func ListenIPC() (net.Listener, error) {
	path := ipcPortPath()
	if path == "" {
		return nil, errors.New("cookies: IPC not supported on this platform")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("cookies: listen tcp loopback: %w", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		ln.Close()
		return nil, fmt.Errorf("cookies: listener returned non-TCP address %T", ln.Addr())
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d", addr.Port)), 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("cookies: write port file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		ln.Close()
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("cookies: publish port file: %w", err)
	}
	return &portFileListener{Listener: ln, portPath: path}, nil
}

// portFileListener removes the port file when the listener closes so
// stale ports don't linger after the native host exits.
type portFileListener struct {
	net.Listener
	portPath string
}

// Close closes the wrapped listener and removes the sidecar port file.
func (l *portFileListener) Close() error {
	err := l.Listener.Close()
	if l.portPath != "" {
		_ = os.Remove(l.portPath)
	}
	return err
}

// LogSink is optionally wired by the plugin so this package can emit
// diagnostic log lines through the Stream Deck connection. Nil is a
// valid value — lines are silently dropped when it's unset.
var LogSink func(string)

// logf is the internal formatter that routes through LogSink.
func logf(format string, args ...any) {
	if LogSink != nil {
		LogSink(fmt.Sprintf("[cookies] "+format, args...))
	}
}

// safeURLForLog returns scheme://host/path from raw, dropping query
// strings and fragments. Cookie-gated providers sometimes encode
// account IDs or session tokens in query params, so the raw URL must
// not land in the persisted Stream Deck log.
func safeURLForLog(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<unparseable>"
	}
	return u.Scheme + "://" + u.Host + u.Path
}

// safeErrForLog collapses an error's text to a short, non-leaky token
// for logs. We keep the type name (e.g. "*net.OpError") and, where
// obvious, a coarse category — anything more detailed could echo
// response bodies or headers back into the log.
func safeErrForLog(err error) string {
	if err == nil {
		return "<nil>"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "i/o timeout"):
		return "i/o timeout"
	case strings.Contains(msg, "connection refused"):
		return "connection refused"
	case strings.Contains(msg, "native host unavailable"):
		return "native host unavailable"
	case strings.Contains(msg, "deadline exceeded"):
		return "deadline exceeded"
	case strings.Contains(msg, "EOF"):
		return "EOF"
	}
	return fmt.Sprintf("%T", err)
}

// safeExtErrForLog collapses an extension-supplied error string to a
// short category, avoiding echoing potentially sensitive response
// snippets into the log.
func safeExtErrForLog(msg string) string {
	switch {
	case strings.Contains(msg, "extension not connected"):
		return "extension not connected"
	case strings.Contains(msg, "origin not allowed"):
		return "origin not allowed"
	case strings.Contains(msg, "timeout"):
		return "timeout"
	case msg == "":
		return "<empty>"
	}
	return "extension error"
}

// requestID is an atomic counter used to mint unique plugin-side
// message IDs for the native-messaging correlation layer.
var requestID atomic.Uint64

// nextRequestID returns a process-unique request correlation string.
func nextRequestID() string {
	return fmt.Sprintf("plug-%d", requestID.Add(1))
}

// dialIPC opens a short-lived TCP loopback connection to the native host.
func dialIPC(ctx context.Context) (net.Conn, error) {
	addr := IPCAddress()
	if addr == "" {
		return nil, ErrHostUnavailable
	}
	dCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dCtx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHostUnavailable, err)
	}
	return conn, nil
}

// roundtrip sends one request frame and reads one response frame.
func roundtrip(ctx context.Context, req Message, timeout time.Duration) (Message, error) {
	conn, err := dialIPC(ctx)
	if err != nil {
		return Message{}, err
	}
	defer conn.Close()
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	payload, err := EncodeMessage(req)
	if err != nil {
		return Message{}, err
	}
	if err := WriteFrame(conn, payload); err != nil {
		return Message{}, err
	}
	raw, err := ReadFrame(conn)
	if err != nil {
		return Message{}, err
	}
	return DecodeMessage(raw)
}

// clientFetch issues a "fetch" message over the IPC socket and decodes
// the extension's reply into a Response.
func clientFetch(ctx context.Context, r Request) (Response, error) {
	method := r.Method
	if method == "" {
		method = "GET"
	}
	reqID := nextRequestID()
	reqMsg := Message{
		ID:      reqID,
		Kind:    "fetch",
		URL:     r.URL,
		Method:  method,
		Headers: r.Headers,
	}
	if len(r.Body) > 0 {
		reqMsg.Body = b64.EncodeToString(r.Body)
	}

	started := time.Now()
	logURL := safeURLForLog(r.URL)
	logf("fetch %s %s %s (body=%dB, timeout=%s)", reqID, method, logURL, len(r.Body), defaultFetchTimeout)

	resp, err := roundtrip(ctx, reqMsg, defaultFetchTimeout)
	elapsed := time.Since(started)
	if err != nil {
		logf("fetch %s failed after %s: %s", reqID, elapsed, safeErrForLog(err))
		return Response{}, err
	}

	if resp.Kind == "error" {
		logf("fetch %s extension-error after %s: %s", reqID, elapsed, safeExtErrForLog(resp.Error))
		if resp.Error == "extension not connected" {
			return Response{}, ErrHostUnavailable
		}
		return Response{}, fmt.Errorf("cookies: extension fetch error: %s", resp.Error)
	}
	if resp.Kind != "fetchResult" {
		logf("fetch %s unexpected-kind %q after %s", reqID, resp.Kind, elapsed)
		return Response{}, fmt.Errorf("cookies: unexpected reply kind %q", resp.Kind)
	}
	var body []byte
	if resp.Body != "" {
		decoded, err := b64.DecodeString(resp.Body)
		if err != nil {
			logf("fetch %s decode-error after %s: %s", reqID, elapsed, safeErrForLog(err))
			return Response{}, fmt.Errorf("cookies: decode response body: %w", err)
		}
		body = decoded
	}
	logf("fetch %s ok status=%d, %d bytes, %s elapsed, ct=%s", reqID, resp.Status, len(body), elapsed, resp.ContentType)
	return Response{
		Status:      resp.Status,
		StatusText:  resp.StatusText,
		Body:        body,
		ContentType: resp.ContentType,
		UserAgent:   resp.UserAgent,
	}, nil
}

// clientStatus returns the current StatusInfo, discarding reachability
// details. Used as probeStatus in this package.
func clientStatus(ctx context.Context) StatusInfo {
	info, _, _ := clientStatusDetail(ctx)
	return info
}

// clientStatusDetail probes the host and returns StatusInfo plus whether
// the socket was reachable and any underlying error.
func clientStatusDetail(ctx context.Context) (StatusInfo, bool, error) {
	resp, err := roundtrip(ctx, Message{ID: nextRequestID(), Kind: "status"}, 750*time.Millisecond)
	if err != nil {
		return StatusInfo{}, false, err
	}
	if resp.Kind != "status" {
		// Reached the host but got an unexpected frame — count as reachable
		// so callers don't blame the socket for a protocol mismatch.
		return StatusInfo{}, true, fmt.Errorf("cookies: unexpected response kind %q", resp.Kind)
	}
	return StatusInfo{
		Ready:        resp.Ready,
		UserAgent:    resp.UserAgent,
		Version:      resp.Version,
		AllowedHosts: append([]string(nil), resp.AllowedHosts...),
	}, true, nil
}

// init wires the package-level transport closures to the TCP-loopback
// implementations once ipc.go is linked in.
func init() {
	dispatchFetch = clientFetch
	probeStatus = clientStatus
	probeStatusDetail = clientStatusDetail
}
