package cookies

import (
	"context"
	"net"
	"sync"
	"time"
)

// pluginExtensionBudget bounds how long the host waits on the
// extension for a reply to a forwarded fetch. Exposed as a var so
// tests can shrink it.
var pluginExtensionBudget = 20 * time.Second

// Bridge wires the browser extension (over stdin/stdout native
// messaging) to the plugin (over the local IPC socket). It tracks
// handshake state, serializes extension writes, and correlates plugin
// requests to extension replies by the request ID.
type Bridge struct {
	mu        sync.Mutex
	ready     bool
	userAgent string
	version   string
	toExt     func(Message) error
	inflight  map[string]chan Message
}

// NewBridge constructs an idle bridge. Wire it into the host by using
// Handle as the native-messaging Handler and calling HandlePluginConn
// for each accepted IPC connection.
func NewBridge() *Bridge {
	return &Bridge{inflight: map[string]chan Message{}}
}

// Handle is the cookies.Handler the native-messaging loop invokes for
// each message from the extension. It also captures the send closure
// so inbound plugin requests can forward messages the other way.
func (b *Bridge) Handle(ctx context.Context, m Message, send func(Message) error) error {
	b.mu.Lock()
	b.toExt = send
	b.mu.Unlock()

	switch m.Kind {
	case "ready":
		b.mu.Lock()
		b.ready = true
		b.userAgent = m.UserAgent
		b.version = m.Version
		b.mu.Unlock()
	case "fetchResult", "error", "pong":
		b.deliverInflight(m)
	}
	return nil
}

// OnExtensionDisconnect is called when the stdin port closes. It
// flips ready to false and releases any plugin connections currently
// waiting on an inflight reply.
func (b *Bridge) OnExtensionDisconnect() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ready = false
	b.toExt = nil
	for id, ch := range b.inflight {
		close(ch)
		delete(b.inflight, id)
	}
}

// StartKeepalive pings the extension on a fixed interval so Chrome's
// service worker idle timer (~30s) keeps resetting. Without this the
// SW suspends, closes the native port, and the host exits — leaving
// the plugin with no bridge until the next chrome.alarms heartbeat
// (up to a minute away). Sends are best-effort; missing toExt / write
// errors are ignored so the ticker keeps running across disconnects.
func (b *Bridge) StartKeepalive(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.mu.Lock()
			send := b.toExt
			b.mu.Unlock()
			if send == nil {
				continue
			}
			_ = send(Message{Kind: "ping"})
		}
	}
}

func (b *Bridge) deliverInflight(m Message) {
	b.mu.Lock()
	ch, ok := b.inflight[m.ID]
	b.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- m:
	default:
	}
}

// HandlePluginConn reads one request frame from conn, services it,
// writes one response frame, and closes.
func (b *Bridge) HandlePluginConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(pluginExtensionBudget + 2*time.Second))

	frame, err := ReadFrame(conn)
	if err != nil {
		return
	}
	req, err := DecodeMessage(frame)
	if err != nil {
		_ = writeMsg(conn, Message{Kind: "error", Error: "malformed request: " + err.Error()})
		return
	}

	switch req.Kind {
	case "status":
		b.mu.Lock()
		resp := Message{
			Kind:      "status",
			Ready:     b.ready,
			UserAgent: b.userAgent,
			Version:   b.version,
		}
		b.mu.Unlock()
		_ = writeMsg(conn, resp)
	case "fetch":
		b.relayFetch(ctx, conn, req)
	default:
		_ = writeMsg(conn, Message{ID: req.ID, Kind: "error", Error: "unknown request kind: " + req.Kind})
	}
}

func (b *Bridge) relayFetch(ctx context.Context, conn net.Conn, req Message) {
	b.mu.Lock()
	if !b.ready || b.toExt == nil {
		b.mu.Unlock()
		_ = writeMsg(conn, Message{ID: req.ID, Kind: "error", Error: "extension not connected"})
		return
	}
	if _, exists := b.inflight[req.ID]; exists {
		b.mu.Unlock()
		_ = writeMsg(conn, Message{ID: req.ID, Kind: "error", Error: "duplicate request id"})
		return
	}
	ch := make(chan Message, 1)
	b.inflight[req.ID] = ch
	send := b.toExt
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.inflight, req.ID)
		b.mu.Unlock()
	}()

	if err := send(req); err != nil {
		_ = writeMsg(conn, Message{ID: req.ID, Kind: "error", Error: "forward to extension: " + err.Error()})
		return
	}

	timer := time.NewTimer(pluginExtensionBudget)
	defer timer.Stop()
	select {
	case resp, ok := <-ch:
		if !ok {
			_ = writeMsg(conn, Message{ID: req.ID, Kind: "error", Error: "extension disconnected"})
			return
		}
		_ = writeMsg(conn, resp)
	case <-timer.C:
		_ = writeMsg(conn, Message{ID: req.ID, Kind: "error", Error: "extension response timeout"})
	case <-ctx.Done():
		_ = writeMsg(conn, Message{ID: req.ID, Kind: "error", Error: "host shutting down"})
	}
}

func writeMsg(conn net.Conn, m Message) error {
	payload, err := EncodeMessage(m)
	if err != nil {
		return err
	}
	return WriteFrame(conn, payload)
}
