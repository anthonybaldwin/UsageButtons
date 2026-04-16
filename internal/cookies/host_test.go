package cookies

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDecodeEncode_RoundTrip(t *testing.T) {
	in := Message{
		ID:     "req-1",
		Kind:   "getCookies",
		Domain: "claude.ai",
		Names:  []string{"sessionKey", "cf_clearance"},
	}
	b, err := EncodeMessage(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := DecodeMessage(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ID != in.ID || out.Kind != in.Kind || out.Domain != in.Domain {
		t.Fatalf("mismatch: %+v vs %+v", out, in)
	}
	if strings.Join(out.Names, ",") != strings.Join(in.Names, ",") {
		t.Fatalf("names mismatch")
	}
}

func TestWireCookie_ToCookie_Session(t *testing.T) {
	w := WireCookie{Name: "s", Value: "v", Domain: "claude.ai", Session: true, ExpirationDate: 1700000000}
	c := w.ToCookie()
	if !c.Expires.IsZero() {
		t.Fatalf("session cookie should have zero expires, got %v", c.Expires)
	}
}

func TestWireCookie_ToCookie_Expiring(t *testing.T) {
	w := WireCookie{Name: "s", Value: "v", Domain: "claude.ai", ExpirationDate: 1700000000.5}
	c := w.ToCookie()
	if c.Expires.IsZero() {
		t.Fatal("expiring cookie should have non-zero expires")
	}
	if c.Expires.Unix() != 1700000000 {
		t.Fatalf("expires: got %v", c.Expires)
	}
}

func TestServeNativeHost_EchoFlow(t *testing.T) {
	// Simulate Chrome: write two framed messages to the host's stdin,
	// then close. Expect two echoed frames on stdout.
	var in bytes.Buffer
	for _, m := range []Message{
		{ID: "1", Kind: "ping"},
		{ID: "2", Kind: "getCookies", Domain: "claude.ai"},
	} {
		payload, err := EncodeMessage(m)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if err := WriteFrame(&in, payload); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := ServeNativeHost(ctx, &in, &out, EchoHandler()); err != nil {
		t.Fatalf("serve: %v", err)
	}

	// Read the two echoed frames back.
	var got []Message
	for {
		frame, err := ReadFrame(&out)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("read frame: %v", err)
		}
		m, err := DecodeMessage(frame)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		got = append(got, m)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 echoed messages, got %d", len(got))
	}
	if got[0].ID != "1" || got[0].Kind != "echo" {
		t.Fatalf("echo 0: %+v", got[0])
	}
	if got[1].ID != "2" || got[1].Kind != "echo" || got[1].Domain != "claude.ai" {
		t.Fatalf("echo 1: %+v", got[1])
	}
}

func TestServeNativeHost_MalformedFrameStaysAlive(t *testing.T) {
	// Manually write a frame with invalid JSON, then a valid ping.
	var in bytes.Buffer
	if err := WriteFrame(&in, []byte("not-json")); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	payload, _ := EncodeMessage(Message{ID: "ok", Kind: "ping"})
	if err := WriteFrame(&in, payload); err != nil {
		t.Fatalf("write frame: %v", err)
	}

	var out bytes.Buffer
	if err := ServeNativeHost(context.Background(), &in, &out, EchoHandler()); err != nil {
		t.Fatalf("serve: %v", err)
	}

	// Expect 2 frames: one error, one echo.
	var kinds []string
	for {
		frame, err := ReadFrame(&out)
		if err != nil {
			break
		}
		m, _ := DecodeMessage(frame)
		kinds = append(kinds, m.Kind)
	}
	if len(kinds) != 2 || kinds[0] != "error" || kinds[1] != "echo" {
		t.Fatalf("kinds: %v", kinds)
	}
}

// handler that records what it received, concurrent-safe.
type recordingHandler struct {
	mu  sync.Mutex
	got []Message
}

func (h *recordingHandler) handle() Handler {
	return func(ctx context.Context, in Message, send func(Message) error) error {
		h.mu.Lock()
		h.got = append(h.got, in)
		h.mu.Unlock()
		return nil
	}
}

func TestServeNativeHost_CtxCancelStopsLoop(t *testing.T) {
	// Pipe with nothing written — ReadFrame will block forever. A
	// cancelled context is checked on the next iteration, but ReadFrame
	// doesn't take ctx, so we have to close the reader to unblock.
	// Verify that a pre-cancelled context AND a closed reader together
	// return promptly (covers the "host told to shut down" path).
	pr, pw := io.Pipe()
	defer pr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Close the write end so ReadFrame gets EOF.
	pw.Close()

	h := &recordingHandler{}
	err := ServeNativeHost(ctx, pr, io.Discard, h.handle())
	// Either nil (EOF first) or ctx.Err (cancel first) is acceptable.
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected err: %v", err)
	}
}
