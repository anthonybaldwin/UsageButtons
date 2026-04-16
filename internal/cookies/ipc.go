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
// a temp path. Both platforms use AF_UNIX — Windows has supported it
// since 10 1803 (April 2018) via net.Listen/Dial("unix", ...).
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
		// Short path — macOS caps sun_path at ~104 bytes. Using /tmp/
		// with the UID suffix keeps the path well under the limit and
		// avoids collisions between users on a shared machine.
		return fmt.Sprintf("/tmp/usagebuttons-%d.sock", os.Getuid())
	default:
		return ""
	}
}

// IPCAddress returns the platform-specific IPC socket path. Mostly
// useful for diagnostic logging in the native host and plugin.
func IPCAddress() string { return ipcAddr() }

// ListenIPC opens the listener the native host uses to serve the
// plugin. Any stale socket file left over from a crashed prior run is
// removed first.
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
// Cookie queries are rare enough that a connection-per-request
// (no pooling) is the right trade — simpler code, cheap on a local
// socket.
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

func clientGet(ctx context.Context, q Query) (Bundle, error) {
	req := Message{
		ID:     nextRequestID(),
		Kind:   "getCookies",
		Domain: q.Domain,
		Names:  q.Names,
	}
	resp, err := roundtrip(ctx, req, 5*time.Second)
	if err != nil {
		return Bundle{}, err
	}
	if resp.Kind == "error" {
		// Host reports "extension not connected" when the SW hasn't
		// handshaken yet. Map that to ErrHostUnavailable so providers
		// stay in the quiet "waiting on browser" state.
		if resp.Error == "extension not connected" {
			return Bundle{}, ErrHostUnavailable
		}
		return Bundle{}, fmt.Errorf("cookies: extension error: %s", resp.Error)
	}
	if resp.Kind != "cookies" {
		return Bundle{}, fmt.Errorf("cookies: unexpected reply kind %q", resp.Kind)
	}
	cs := make([]Cookie, 0, len(resp.Cookies))
	for _, w := range resp.Cookies {
		cs = append(cs, w.ToCookie())
	}
	return Bundle{Cookies: cs, UserAgent: resp.UserAgent}, nil
}

func clientProbe(ctx context.Context) bool {
	resp, err := roundtrip(ctx, Message{ID: nextRequestID(), Kind: "status"}, 750*time.Millisecond)
	if err != nil {
		return false
	}
	return resp.Kind == "status" && resp.Ready
}

func init() {
	dispatchGet = clientGet
	probeHost = clientProbe
}
