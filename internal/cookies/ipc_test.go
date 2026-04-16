package cookies

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
)

// setupTestIPC spins up a unix-socket listener at a temp path and
// points ipcAddr at it for the test's lifetime. handler runs per
// accepted connection.
func setupTestIPC(t *testing.T, handler func(net.Conn)) {
	t.Helper()
	tmp := t.TempDir()
	sock := filepath.Join(tmp, "t.sock")

	prev := ipcAddr
	t.Cleanup(func() { ipcAddr = prev })
	ipcAddr = func() string { return sock }

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket listen unsupported: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handler(conn)
		}
	}()
}

func pointIPCAtMissing(t *testing.T) {
	t.Helper()
	prev := ipcAddr
	t.Cleanup(func() { ipcAddr = prev })
	tmp := t.TempDir()
	ipcAddr = func() string { return filepath.Join(tmp, "missing.sock") }
}

func TestHostAvailable_NoListener(t *testing.T) {
	pointIPCAtMissing(t)
	if HostAvailable(context.Background()) {
		t.Fatal("HostAvailable should be false when nothing listens")
	}
}

func TestFetch_NoListenerReturnsUnavailable(t *testing.T) {
	pointIPCAtMissing(t)
	_, err := Fetch(context.Background(), Request{URL: "https://claude.ai/api/organizations"})
	if !errors.Is(err, ErrHostUnavailable) {
		t.Fatalf("want ErrHostUnavailable, got %v", err)
	}
}

func TestFetchJSON_Success(t *testing.T) {
	setupTestIPC(t, func(conn net.Conn) {
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		frame, err := ReadFrame(conn)
		if err != nil {
			return
		}
		req, _ := DecodeMessage(frame)
		body := []byte(`{"hello":"world"}`)
		resp := Message{
			ID:          req.ID,
			Kind:        "fetchResult",
			Status:      200,
			StatusText:  "OK",
			ContentType: "application/json",
			Body:        base64.StdEncoding.EncodeToString(body),
			UserAgent:   "Mozilla/5.0 test",
		}
		payload, _ := EncodeMessage(resp)
		_ = WriteFrame(conn, payload)
	})

	var out struct {
		Hello string `json:"hello"`
	}
	err := FetchJSON(context.Background(), "https://claude.ai/api/x", nil, &out)
	if err != nil {
		t.Fatalf("FetchJSON: %v", err)
	}
	if out.Hello != "world" {
		t.Fatalf("decoded: %+v", out)
	}
}

func TestFetch_PropagatesNon2xxAsHTTPError(t *testing.T) {
	setupTestIPC(t, func(conn net.Conn) {
		defer conn.Close()
		frame, _ := ReadFrame(conn)
		req, _ := DecodeMessage(frame)
		resp := Message{
			ID:         req.ID,
			Kind:       "fetchResult",
			Status:     401,
			StatusText: "Unauthorized",
			Body:       base64.StdEncoding.EncodeToString([]byte("nope")),
		}
		payload, _ := EncodeMessage(resp)
		_ = WriteFrame(conn, payload)
	})

	err := FetchJSON(context.Background(), "https://claude.ai/api/x", nil, new(map[string]any))
	var httpErr *httputil.Error
	if !errors.As(err, &httpErr) {
		t.Fatalf("want *httputil.Error, got %T / %v", err, err)
	}
	if httpErr.Status != 401 {
		t.Fatalf("status: %d", httpErr.Status)
	}
}

func TestFetchHTML_Success(t *testing.T) {
	setupTestIPC(t, func(conn net.Conn) {
		defer conn.Close()
		frame, _ := ReadFrame(conn)
		req, _ := DecodeMessage(frame)
		body := []byte("<html><body>hi</body></html>")
		resp := Message{
			ID:     req.ID,
			Kind:   "fetchResult",
			Status: 200,
			Body:   base64.StdEncoding.EncodeToString(body),
		}
		payload, _ := EncodeMessage(resp)
		_ = WriteFrame(conn, payload)
	})

	html, err := FetchHTML(context.Background(), "https://ollama.com/settings", nil)
	if err != nil {
		t.Fatalf("FetchHTML: %v", err)
	}
	if html != "<html><body>hi</body></html>" {
		t.Fatalf("html: %q", html)
	}
}

func TestFetch_ExtensionNotConnectedMapsToUnavailable(t *testing.T) {
	setupTestIPC(t, func(conn net.Conn) {
		defer conn.Close()
		frame, _ := ReadFrame(conn)
		req, _ := DecodeMessage(frame)
		resp := Message{ID: req.ID, Kind: "error", Error: "extension not connected"}
		payload, _ := EncodeMessage(resp)
		_ = WriteFrame(conn, payload)
	})

	_, err := Fetch(context.Background(), Request{URL: "https://claude.ai/"})
	if !errors.Is(err, ErrHostUnavailable) {
		t.Fatalf("want ErrHostUnavailable, got %v", err)
	}
}

func TestFetch_GenericExtensionErrorSurfaces(t *testing.T) {
	setupTestIPC(t, func(conn net.Conn) {
		defer conn.Close()
		frame, _ := ReadFrame(conn)
		req, _ := DecodeMessage(frame)
		resp := Message{ID: req.ID, Kind: "error", Error: "some transient failure"}
		payload, _ := EncodeMessage(resp)
		_ = WriteFrame(conn, payload)
	})

	_, err := Fetch(context.Background(), Request{URL: "https://claude.ai/"})
	if err == nil {
		t.Fatal("want error")
	}
	if errors.Is(err, ErrHostUnavailable) {
		t.Fatalf("generic errors should NOT map to unavailable: %v", err)
	}
}

func TestClientProbe_Ready(t *testing.T) {
	setupTestIPC(t, func(conn net.Conn) {
		defer conn.Close()
		frame, _ := ReadFrame(conn)
		req, _ := DecodeMessage(frame)
		resp := Message{ID: req.ID, Kind: "status", Ready: true, UserAgent: "UA"}
		payload, _ := EncodeMessage(resp)
		_ = WriteFrame(conn, payload)
	})

	if !HostAvailable(context.Background()) {
		t.Fatal("HostAvailable should be true when host replies Ready=true")
	}
}

func TestClientProbe_NotReady(t *testing.T) {
	setupTestIPC(t, func(conn net.Conn) {
		defer conn.Close()
		frame, _ := ReadFrame(conn)
		req, _ := DecodeMessage(frame)
		resp := Message{ID: req.ID, Kind: "status", Ready: false}
		payload, _ := EncodeMessage(resp)
		_ = WriteFrame(conn, payload)
	})

	if HostAvailable(context.Background()) {
		t.Fatal("HostAvailable should be false when host replies Ready=false")
	}
}

func TestListenIPC_RemovesStaleSocket(t *testing.T) {
	tmp := t.TempDir()
	sock := filepath.Join(tmp, "stale.sock")

	prev := ipcAddr
	t.Cleanup(func() { ipcAddr = prev })
	ipcAddr = func() string { return sock }

	ln1, err := ListenIPC()
	if err != nil {
		t.Skipf("unix socket unsupported: %v", err)
	}
	_ = ln1.Close()

	ln2, err := ListenIPC()
	if err != nil {
		t.Fatalf("second Listen: %v", err)
	}
	defer ln2.Close()
}
