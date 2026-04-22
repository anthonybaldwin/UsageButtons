package cookies

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func TestDecodeEncode_RoundTrip(t *testing.T) {
	in := Message{
		ID:     "req-1",
		Kind:   "fetch",
		URL:    "https://claude.ai/api/x",
		Method: "GET",
		Headers: map[string]string{
			"Accept": "application/json",
		},
	}
	b, err := EncodeMessage(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := DecodeMessage(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ID != in.ID || out.Kind != in.Kind || out.URL != in.URL {
		t.Fatalf("mismatch: %+v vs %+v", out, in)
	}
	if out.Headers["Accept"] != "application/json" {
		t.Fatalf("headers: %v", out.Headers)
	}
}

func TestServeNativeHost_EchoFlow(t *testing.T) {
	var in bytes.Buffer
	for _, m := range []Message{
		{ID: "1", Kind: "ping"},
		{ID: "2", Kind: "fetch", URL: "https://claude.ai/"},
	} {
		payload, _ := EncodeMessage(m)
		if err := WriteFrame(&in, payload); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := ServeNativeHost(ctx, &in, &out, echoHandler()); err != nil {
		t.Fatalf("serve: %v", err)
	}

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
	if got[1].ID != "2" || got[1].Kind != "echo" || got[1].URL != "https://claude.ai/" {
		t.Fatalf("echo 1: %+v", got[1])
	}
}

func TestServeNativeHost_MalformedFrameStaysAlive(t *testing.T) {
	var in bytes.Buffer
	if err := WriteFrame(&in, []byte("not-json")); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	payload, _ := EncodeMessage(Message{ID: "ok", Kind: "ping"})
	if err := WriteFrame(&in, payload); err != nil {
		t.Fatalf("write frame: %v", err)
	}

	var out bytes.Buffer
	if err := ServeNativeHost(context.Background(), &in, &out, echoHandler()); err != nil {
		t.Fatalf("serve: %v", err)
	}

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
	pr, pw := io.Pipe()
	defer pr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pw.Close()

	h := &recordingHandler{}
	err := ServeNativeHost(ctx, pr, io.Discard, h.handle())
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected err: %v", err)
	}
}

// echoHandler returns inbound messages verbatim with Kind="echo". Used
// by message-loop tests in isolation.
func echoHandler() Handler {
	return func(_ context.Context, in Message, send func(Message) error) error {
		out := in
		out.Kind = "echo"
		return send(out)
	}
}
