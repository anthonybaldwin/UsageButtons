package cookies

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"
)

// ipcAddr returns the local IPC socket path shared by the native host
// (listener) and the plugin (dialer). It's a var so tests can swap in
// a temp path.
var ipcAddr = defaultIPCAddr

func defaultIPCAddr() string {
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = os.TempDir()
		}
		dir := filepath.Join(base, "UsageButtons")
		_ = os.MkdirAll(dir, 0o755)
		return filepath.Join(dir, "ipc.sock")
	case "darwin":
		return fmt.Sprintf("/tmp/usagebuttons-%d.sock", os.Getuid())
	default:
		return ""
	}
}

// IPCAddress returns the platform-specific IPC socket path. Mostly
// useful for diagnostic logging.
func IPCAddress() string { return ipcAddr() }

// ListenIPC opens the listener the native host uses to serve the
// plugin. Removes any stale socket file from a crashed prior run.
func ListenIPC() (net.Listener, error) {
	addr := ipcAddr()
	if addr == "" {
		return nil, errors.New("cookies: IPC not supported on this platform")
	}
	_ = os.Remove(addr)
	return net.Listen("unix", addr)
}

var requestID atomic.Uint64

func nextRequestID() string {
	return fmt.Sprintf("plug-%d", requestID.Add(1))
}

func dialIPC(ctx context.Context) (net.Conn, error) {
	addr := ipcAddr()
	if addr == "" {
		return nil, ErrHostUnavailable
	}
	dCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dCtx, "unix", addr)
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

func clientFetch(ctx context.Context, r Request) (Response, error) {
	method := r.Method
	if method == "" {
		method = "GET"
	}
	reqMsg := Message{
		ID:      nextRequestID(),
		Kind:    "fetch",
		URL:     r.URL,
		Method:  method,
		Headers: r.Headers,
	}
	if len(r.Body) > 0 {
		reqMsg.Body = b64.EncodeToString(r.Body)
	}

	resp, err := roundtrip(ctx, reqMsg, defaultFetchTimeout)
	if err != nil {
		return Response{}, err
	}

	if resp.Kind == "error" {
		if resp.Error == "extension not connected" {
			return Response{}, ErrHostUnavailable
		}
		return Response{}, fmt.Errorf("cookies: extension fetch error: %s", resp.Error)
	}
	if resp.Kind != "fetchResult" {
		return Response{}, fmt.Errorf("cookies: unexpected reply kind %q", resp.Kind)
	}

	var body []byte
	if resp.Body != "" {
		decoded, err := b64.DecodeString(resp.Body)
		if err != nil {
			return Response{}, fmt.Errorf("cookies: decode response body: %w", err)
		}
		body = decoded
	}
	return Response{
		Status:      resp.Status,
		StatusText:  resp.StatusText,
		Body:        body,
		ContentType: resp.ContentType,
		UserAgent:   resp.UserAgent,
	}, nil
}

func clientStatus(ctx context.Context) StatusInfo {
	resp, err := roundtrip(ctx, Message{ID: nextRequestID(), Kind: "status"}, 750*time.Millisecond)
	if err != nil || resp.Kind != "status" {
		return StatusInfo{}
	}
	return StatusInfo{Ready: resp.Ready, UserAgent: resp.UserAgent, Version: resp.Version}
}

func init() {
	dispatchFetch = clientFetch
	probeStatus = clientStatus
}
