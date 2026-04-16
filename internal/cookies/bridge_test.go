package cookies

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// extStub simulates the Chrome extension. Captures outbound messages
// (host→ext) on a channel; tests reply by calling bridge.Handle.
type extStub struct {
	out chan Message
	err error
}

func newExtStub() *extStub {
	return &extStub{out: make(chan Message, 8)}
}

func (e *extStub) send(m Message) error {
	if e.err != nil {
		return e.err
	}
	e.out <- m
	return nil
}

func roundtripPluginConn(t *testing.T, bridge *Bridge, req Message) Message {
	t.Helper()
	plugSide, hostSide := net.Pipe()
	defer plugSide.Close()
	go bridge.HandlePluginConn(context.Background(), hostSide)

	payload, err := EncodeMessage(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	_ = plugSide.SetDeadline(time.Now().Add(2 * time.Second))
	if err := WriteFrame(plugSide, payload); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	frame, err := ReadFrame(plugSide)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	resp, err := DecodeMessage(frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestBridge_StatusBeforeReady(t *testing.T) {
	b := NewBridge()
	resp := roundtripPluginConn(t, b, Message{Kind: "status"})
	if resp.Kind != "status" || resp.Ready {
		t.Fatalf("want kind=status ready=false, got %+v", resp)
	}
}

func TestBridge_StatusAfterReady(t *testing.T) {
	b := NewBridge()
	ext := newExtStub()
	_ = b.Handle(context.Background(), Message{Kind: "ready", UserAgent: "UA-X", Version: "0.1.0"}, ext.send)

	resp := roundtripPluginConn(t, b, Message{Kind: "status"})
	if !resp.Ready || resp.UserAgent != "UA-X" || resp.Version != "0.1.0" {
		t.Fatalf("status: %+v", resp)
	}
}

func TestBridge_GetCookies_ExtensionNotConnected(t *testing.T) {
	b := NewBridge()
	resp := roundtripPluginConn(t, b, Message{ID: "p-1", Kind: "getCookies", Domain: "claude.ai"})
	if resp.Kind != "error" || resp.Error != "extension not connected" {
		t.Fatalf("got %+v", resp)
	}
}

func TestBridge_GetCookies_RoundTrip(t *testing.T) {
	b := NewBridge()
	ext := newExtStub()
	_ = b.Handle(context.Background(), Message{Kind: "ready", UserAgent: "UA-X"}, ext.send)

	plugSide, hostSide := net.Pipe()
	defer plugSide.Close()
	go b.HandlePluginConn(context.Background(), hostSide)

	// Send the plugin request.
	payload, _ := EncodeMessage(Message{ID: "p-42", Kind: "getCookies", Domain: "claude.ai"})
	_ = plugSide.SetDeadline(time.Now().Add(3 * time.Second))
	if err := WriteFrame(plugSide, payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Bridge should forward to extension with the same ID.
	select {
	case forwarded := <-ext.out:
		if forwarded.ID != "p-42" || forwarded.Kind != "getCookies" || forwarded.Domain != "claude.ai" {
			t.Fatalf("forwarded: %+v", forwarded)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not forward to extension")
	}

	// Simulate the extension's reply arriving on the stdin loop.
	reply := Message{
		ID:        "p-42",
		Kind:      "cookies",
		UserAgent: "UA-X",
		Cookies: []WireCookie{
			{Name: "sessionKey", Value: "s", Domain: "claude.ai"},
		},
	}
	_ = b.Handle(context.Background(), reply, ext.send)

	frame, err := ReadFrame(plugSide)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got, _ := DecodeMessage(frame)
	if got.ID != "p-42" || got.Kind != "cookies" || len(got.Cookies) != 1 {
		t.Fatalf("reply: %+v", got)
	}
}

func TestBridge_GetCookies_DisconnectReleasesWaiters(t *testing.T) {
	b := NewBridge()
	ext := newExtStub()
	_ = b.Handle(context.Background(), Message{Kind: "ready", UserAgent: "UA"}, ext.send)

	plugSide, hostSide := net.Pipe()
	defer plugSide.Close()
	go b.HandlePluginConn(context.Background(), hostSide)

	payload, _ := EncodeMessage(Message{ID: "p-1", Kind: "getCookies", Domain: "claude.ai"})
	_ = plugSide.SetDeadline(time.Now().Add(2 * time.Second))
	if err := WriteFrame(plugSide, payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	<-ext.out // drain the forward

	// Extension dies before it can reply.
	b.OnExtensionDisconnect()

	frame, err := ReadFrame(plugSide)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got, _ := DecodeMessage(frame)
	if got.Kind != "error" || got.Error != "extension disconnected" {
		t.Fatalf("want extension disconnected, got %+v", got)
	}
}

func TestBridge_GetCookies_Timeout(t *testing.T) {
	prev := pluginExtensionBudget
	t.Cleanup(func() { pluginExtensionBudget = prev })
	pluginExtensionBudget = 100 * time.Millisecond

	b := NewBridge()
	ext := newExtStub()
	_ = b.Handle(context.Background(), Message{Kind: "ready", UserAgent: "UA"}, ext.send)

	plugSide, hostSide := net.Pipe()
	defer plugSide.Close()
	go b.HandlePluginConn(context.Background(), hostSide)

	payload, _ := EncodeMessage(Message{ID: "p-1", Kind: "getCookies", Domain: "claude.ai"})
	_ = plugSide.SetDeadline(time.Now().Add(2 * time.Second))
	if err := WriteFrame(plugSide, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	<-ext.out // drain

	frame, err := ReadFrame(plugSide)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got, _ := DecodeMessage(frame)
	if got.Kind != "error" || got.Error != "extension response timeout" {
		t.Fatalf("want timeout error, got %+v", got)
	}
}

func TestBridge_Handle_ReadySetsUserAgent(t *testing.T) {
	b := NewBridge()
	ext := newExtStub()
	err := b.Handle(context.Background(), Message{Kind: "ready", UserAgent: "UA-Y"}, ext.send)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.ready || b.userAgent != "UA-Y" {
		t.Fatalf("ready/UA: %v / %q", b.ready, b.userAgent)
	}
}

func TestBridge_HandleForwardSendError(t *testing.T) {
	b := NewBridge()
	ext := &extStub{err: errors.New("broken pipe"), out: make(chan Message, 1)}
	_ = b.Handle(context.Background(), Message{Kind: "ready", UserAgent: "UA"}, ext.send)

	resp := roundtripPluginConn(t, b, Message{ID: "p-1", Kind: "getCookies", Domain: "claude.ai"})
	if resp.Kind != "error" {
		t.Fatalf("want error, got %+v", resp)
	}
}
