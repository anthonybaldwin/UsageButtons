package cookies

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"testing"
	"time"
)

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

func TestBridge_Fetch_ExtensionNotConnected(t *testing.T) {
	b := NewBridge()
	resp := roundtripPluginConn(t, b, Message{ID: "p-1", Kind: "fetch", URL: "https://claude.ai/"})
	if resp.Kind != "error" || resp.Error != "extension not connected" {
		t.Fatalf("got %+v", resp)
	}
}

func TestBridge_Fetch_RoundTrip(t *testing.T) {
	b := NewBridge()
	ext := newExtStub()
	_ = b.Handle(context.Background(), Message{Kind: "ready", UserAgent: "UA-X"}, ext.send)

	plugSide, hostSide := net.Pipe()
	defer plugSide.Close()
	go b.HandlePluginConn(context.Background(), hostSide)

	payload, _ := EncodeMessage(Message{ID: "p-42", Kind: "fetch", URL: "https://claude.ai/api/x"})
	_ = plugSide.SetDeadline(time.Now().Add(3 * time.Second))
	if err := WriteFrame(plugSide, payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case forwarded := <-ext.out:
		if forwarded.ID != "p-42" || forwarded.Kind != "fetch" || forwarded.URL != "https://claude.ai/api/x" {
			t.Fatalf("forwarded: %+v", forwarded)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not forward to extension")
	}

	reply := Message{
		ID:         "p-42",
		Kind:       "fetchResult",
		Status:     200,
		StatusText: "OK",
		Body:       base64.StdEncoding.EncodeToString([]byte(`{"ok":true}`)),
	}
	_ = b.Handle(context.Background(), reply, ext.send)

	frame, err := ReadFrame(plugSide)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got, _ := DecodeMessage(frame)
	if got.ID != "p-42" || got.Kind != "fetchResult" || got.Status != 200 {
		t.Fatalf("reply: %+v", got)
	}
}

func TestBridge_Fetch_DisconnectReleasesWaiters(t *testing.T) {
	b := NewBridge()
	ext := newExtStub()
	_ = b.Handle(context.Background(), Message{Kind: "ready", UserAgent: "UA"}, ext.send)

	plugSide, hostSide := net.Pipe()
	defer plugSide.Close()
	go b.HandlePluginConn(context.Background(), hostSide)

	payload, _ := EncodeMessage(Message{ID: "p-1", Kind: "fetch", URL: "https://claude.ai/"})
	_ = plugSide.SetDeadline(time.Now().Add(2 * time.Second))
	if err := WriteFrame(plugSide, payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	<-ext.out

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

func TestBridge_Fetch_Timeout(t *testing.T) {
	prev := pluginExtensionBudget
	t.Cleanup(func() { pluginExtensionBudget = prev })
	pluginExtensionBudget = 100 * time.Millisecond

	b := NewBridge()
	ext := newExtStub()
	_ = b.Handle(context.Background(), Message{Kind: "ready", UserAgent: "UA"}, ext.send)

	plugSide, hostSide := net.Pipe()
	defer plugSide.Close()
	go b.HandlePluginConn(context.Background(), hostSide)

	payload, _ := EncodeMessage(Message{ID: "p-1", Kind: "fetch", URL: "https://claude.ai/"})
	_ = plugSide.SetDeadline(time.Now().Add(2 * time.Second))
	if err := WriteFrame(plugSide, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	<-ext.out

	frame, err := ReadFrame(plugSide)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got, _ := DecodeMessage(frame)
	if got.Kind != "error" || got.Error != "extension response timeout" {
		t.Fatalf("want timeout error, got %+v", got)
	}
}

func TestBridge_HandleForwardSendError(t *testing.T) {
	b := NewBridge()
	ext := &extStub{err: errors.New("broken pipe"), out: make(chan Message, 1)}
	_ = b.Handle(context.Background(), Message{Kind: "ready", UserAgent: "UA"}, ext.send)

	resp := roundtripPluginConn(t, b, Message{ID: "p-1", Kind: "fetch", URL: "https://claude.ai/"})
	if resp.Kind != "error" {
		t.Fatalf("want error, got %+v", resp)
	}
}
