package cookies

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// setupTestIPC spins up a unix-socket listener at a temp path and
// points ipcAddr at it for the test's lifetime. handler runs per
// accepted connection; it should read a single frame, write a reply,
// then close.
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

// Replace the default-unavailable test with a listener-less IPC target
// so the test is deterministic regardless of whether the dev machine
// happens to have a real native host running.
func TestHostAvailable_NoListener(t *testing.T) {
	pointIPCAtMissing(t)
	if HostAvailable(context.Background()) {
		t.Fatal("HostAvailable should be false when nothing listens")
	}
}

func TestGet_NoListenerReturnsUnavailable(t *testing.T) {
	pointIPCAtMissing(t)
	_, err := Get(context.Background(), Query{Domain: "claude.ai"})
	if !errors.Is(err, ErrHostUnavailable) {
		t.Fatalf("want ErrHostUnavailable, got %v", err)
	}
}

func TestClientGet_Success(t *testing.T) {
	setupTestIPC(t, func(conn net.Conn) {
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		frame, err := ReadFrame(conn)
		if err != nil {
			return
		}
		req, _ := DecodeMessage(frame)
		resp := Message{
			ID:        req.ID,
			Kind:      "cookies",
			UserAgent: "Mozilla/5.0 test",
			Cookies: []WireCookie{
				{Name: "sessionKey", Value: "v1", Domain: "claude.ai", Path: "/"},
				{Name: "cf_clearance", Value: "v2", Domain: ".claude.ai", Secure: true},
			},
		}
		payload, _ := EncodeMessage(resp)
		_ = WriteFrame(conn, payload)
	})

	b, err := Get(context.Background(), Query{Domain: "claude.ai"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(b.Cookies) != 2 {
		t.Fatalf("cookies: got %d, want 2", len(b.Cookies))
	}
	if b.UserAgent != "Mozilla/5.0 test" {
		t.Fatalf("ua: got %q", b.UserAgent)
	}
	if b.Cookies[1].Name != "cf_clearance" || !b.Cookies[1].Secure {
		t.Fatalf("cf_clearance: %+v", b.Cookies[1])
	}
}

func TestClientGet_HeaderHelperJoinsCookies(t *testing.T) {
	setupTestIPC(t, func(conn net.Conn) {
		defer conn.Close()
		frame, _ := ReadFrame(conn)
		req, _ := DecodeMessage(frame)
		resp := Message{
			ID:        req.ID,
			Kind:      "cookies",
			UserAgent: "UA-X",
			Cookies: []WireCookie{
				{Name: "a", Value: "1"},
				{Name: "b", Value: "2"},
			},
		}
		payload, _ := EncodeMessage(resp)
		_ = WriteFrame(conn, payload)
	})

	h, ua, err := Header(context.Background(), Query{Domain: "claude.ai"})
	if err != nil {
		t.Fatalf("Header: %v", err)
	}
	if h != "a=1; b=2" {
		t.Fatalf("header: got %q", h)
	}
	if ua != "UA-X" {
		t.Fatalf("ua: got %q", ua)
	}
}

func TestClientGet_ExtensionNotConnectedMapsToUnavailable(t *testing.T) {
	setupTestIPC(t, func(conn net.Conn) {
		defer conn.Close()
		frame, _ := ReadFrame(conn)
		req, _ := DecodeMessage(frame)
		resp := Message{ID: req.ID, Kind: "error", Error: "extension not connected"}
		payload, _ := EncodeMessage(resp)
		_ = WriteFrame(conn, payload)
	})

	_, err := Get(context.Background(), Query{Domain: "claude.ai"})
	if !errors.Is(err, ErrHostUnavailable) {
		t.Fatalf("want ErrHostUnavailable, got %v", err)
	}
}

func TestClientGet_GenericExtensionErrorSurfaces(t *testing.T) {
	setupTestIPC(t, func(conn net.Conn) {
		defer conn.Close()
		frame, _ := ReadFrame(conn)
		req, _ := DecodeMessage(frame)
		resp := Message{ID: req.ID, Kind: "error", Error: "some transient failure"}
		payload, _ := EncodeMessage(resp)
		_ = WriteFrame(conn, payload)
	})

	_, err := Get(context.Background(), Query{Domain: "claude.ai"})
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

// Guard the listener path itself — ListenIPC must remove a stale
// socket file rather than returning EADDRINUSE.
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
	// Leave the file in place to simulate a crashed prior run.

	ln2, err := ListenIPC()
	if err != nil {
		t.Fatalf("second Listen: %v", err)
	}
	defer ln2.Close()
}
