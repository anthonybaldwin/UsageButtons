package cookies

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
)

// setupTestIPC spins up a TCP loopback listener on an ephemeral port,
// writes the port to a temp sidecar file, and points ipcPortPath at
// it for the test's lifetime. handler runs per accepted connection.
func setupTestIPC(t *testing.T, handler func(net.Conn)) {
	t.Helper()
	tmp := t.TempDir()
	portFile := filepath.Join(tmp, "ipc.port")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp loopback listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	port := ln.Addr().(*net.TCPAddr).Port
	if err := os.WriteFile(portFile, []byte(fmt.Sprintf("%d", port)), 0o600); err != nil {
		t.Fatalf("write port file: %v", err)
	}

	prev := ipcPortPath
	t.Cleanup(func() { ipcPortPath = prev })
	ipcPortPath = func() string { return portFile }

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
	prev := ipcPortPath
	t.Cleanup(func() { ipcPortPath = prev })
	tmp := t.TempDir()
	ipcPortPath = func() string { return filepath.Join(tmp, "missing.port") }
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
		resp := Message{ID: req.ID, Kind: "status", Ready: true, UserAgent: "UA", AllowedHosts: Allowed}
		payload, _ := EncodeMessage(resp)
		_ = WriteFrame(conn, payload)
	})

	if !HostAvailable(context.Background()) {
		t.Fatal("HostAvailable should be true when host replies Ready=true")
	}
}

func TestClientProbe_StaleAllowlist(t *testing.T) {
	setupTestIPC(t, func(conn net.Conn) {
		defer conn.Close()
		frame, _ := ReadFrame(conn)
		req, _ := DecodeMessage(frame)
		resp := Message{ID: req.ID, Kind: "status", Ready: true, UserAgent: "UA"}
		payload, _ := EncodeMessage(resp)
		_ = WriteFrame(conn, payload)
	})

	if !HostAvailable(context.Background()) {
		t.Fatal("HostAvailable should stay true when helper omits allowlist")
	}
	status := Status(context.Background())
	if HelperAllowlistCurrent(status.AllowedHosts) {
		t.Fatal("HelperAllowlistCurrent should be false when helper omits allowlist")
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

func TestReprime_RoundTrip(t *testing.T) {
	resetReprimeStateForTest()
	t.Cleanup(resetReprimeStateForTest)

	gotURL := make(chan string, 1)
	setupTestIPC(t, func(conn net.Conn) {
		defer conn.Close()
		frame, err := ReadFrame(conn)
		if err != nil {
			return
		}
		req, _ := DecodeMessage(frame)
		gotURL <- req.URL
		resp := Message{ID: req.ID, Kind: "reprimeResult"}
		payload, _ := EncodeMessage(resp)
		_ = WriteFrame(conn, payload)
	})

	if err := Reprime(context.Background(), "https://portal.nousresearch.com/usage"); err != nil {
		t.Fatalf("Reprime: %v", err)
	}
	select {
	case url := <-gotURL:
		if url != "https://portal.nousresearch.com/usage" {
			t.Fatalf("forwarded url: %q", url)
		}
	case <-time.After(time.Second):
		t.Fatal("ipc handler never saw the request")
	}
}

func TestReprime_RateLimited(t *testing.T) {
	resetReprimeStateForTest()
	t.Cleanup(resetReprimeStateForTest)

	hits := make(chan struct{}, 4)
	setupTestIPC(t, func(conn net.Conn) {
		defer conn.Close()
		frame, err := ReadFrame(conn)
		if err != nil {
			return
		}
		req, _ := DecodeMessage(frame)
		hits <- struct{}{}
		resp := Message{ID: req.ID, Kind: "reprimeResult"}
		payload, _ := EncodeMessage(resp)
		_ = WriteFrame(conn, payload)
	})

	if err := Reprime(context.Background(), "https://portal.nousresearch.com/usage"); err != nil {
		t.Fatalf("first Reprime: %v", err)
	}
	if err := Reprime(context.Background(), "https://portal.nousresearch.com/usage"); !errors.Is(err, ErrReprimeRateLimited) {
		t.Fatalf("second Reprime should be rate-limited, got %v", err)
	}
	// Confirm we hit the host exactly once.
	<-hits
	select {
	case <-hits:
		t.Fatal("rate-limited call should not have reached the host")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestReprime_OriginNotAllowed(t *testing.T) {
	resetReprimeStateForTest()
	t.Cleanup(resetReprimeStateForTest)

	err := Reprime(context.Background(), "https://example.com/")
	if !errors.Is(err, ErrOriginNotAllowed) {
		t.Fatalf("want ErrOriginNotAllowed, got %v", err)
	}
}

func TestListenIPC_PublishesPortAndCleansOnClose(t *testing.T) {
	tmp := t.TempDir()
	portFile := filepath.Join(tmp, "ipc.port")

	prev := ipcPortPath
	t.Cleanup(func() { ipcPortPath = prev })
	ipcPortPath = func() string { return portFile }

	ln1, err := ListenIPC()
	if err != nil {
		t.Fatalf("ListenIPC: %v", err)
	}
	data, err := os.ReadFile(portFile)
	if err != nil {
		t.Fatalf("port file not written: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("port file is empty")
	}
	addr := IPCAddress()
	if addr == "" {
		t.Fatal("IPCAddress returned empty after ListenIPC")
	}
	_ = ln1.Close()

	if _, err := os.Stat(portFile); !os.IsNotExist(err) {
		t.Fatalf("port file should be removed on Close, stat err: %v", err)
	}

	// A second listener can rebind fresh without any stale-state shenanigans.
	ln2, err := ListenIPC()
	if err != nil {
		t.Fatalf("second Listen: %v", err)
	}
	defer ln2.Close()
}
