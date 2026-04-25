package cookies

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// Message is the tagged-union envelope exchanged between the native
// host and the extension's service worker (and the plugin ↔ host IPC).
// Only fields relevant to Kind are populated; the rest serialize as
// omitempty.
//
// Host → extension kinds: "fetch", "ping".
// Extension → host kinds: "ready", "fetchResult", "pong", "error".
// Plugin ↔ host kinds: "status" (Ready flag), "fetch" / "fetchResult"
// relayed through to/from the extension.
type Message struct {
	ID      string            `json:"id,omitempty"`
	Kind    string            `json:"kind"`
	URL     string            `json:"url,omitempty"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Body carries the base64-encoded request or response body,
	// direction implied by Kind. Base64 keeps binary bytes + UTF-8
	// text alike JSON-safe.
	Body        string `json:"body,omitempty"`
	Status      int    `json:"status,omitempty"`
	StatusText  string `json:"statusText,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	UserAgent   string `json:"userAgent,omitempty"`
	Version     string `json:"version,omitempty"`
	Error       string `json:"error,omitempty"`
	Ready       bool   `json:"ready,omitempty"`
}

// DecodeMessage parses a native-messaging frame payload.
func DecodeMessage(data []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		return Message{}, fmt.Errorf("cookies: decode: %w", err)
	}
	return m, nil
}

// EncodeMessage marshals a Message to frame payload bytes.
func EncodeMessage(m Message) ([]byte, error) {
	return json.Marshal(m)
}

// Handler reacts to inbound native-messaging messages. send is safe
// for concurrent use; call it zero or more times per inbound message.
type Handler func(ctx context.Context, in Message, send func(Message) error) error

// ServeNativeHost runs Chrome's stdin/stdout native-messaging loop,
// invoking handle for each inbound message. Returns nil on clean EOF
// (port closed by browser) and an error on framing or I/O failure.
func ServeNativeHost(ctx context.Context, r io.Reader, w io.Writer, handle Handler) error {
	var mu sync.Mutex
	send := func(m Message) error {
		payload, err := EncodeMessage(m)
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		return WriteFrame(w, payload)
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		frame, err := ReadFrame(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		msg, err := DecodeMessage(frame)
		if err != nil {
			_ = send(Message{Kind: "error", Error: err.Error()})
			continue
		}
		if err := handle(ctx, msg, send); err != nil {
			return err
		}
	}
}

// LogPath returns a sensible sidecar log path for the native host. We
// can't log to stdout — the browser owns it.
func LogPath() string {
	var dir string
	switch runtime.GOOS {
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			dir = filepath.Join(v, "UsageButtons")
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, "Library", "Logs", "UsageButtons")
		}
	}
	if dir == "" {
		dir = os.TempDir()
	}
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "native-host.log")
}
