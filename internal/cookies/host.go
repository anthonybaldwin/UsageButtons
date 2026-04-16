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
	"time"
)

// Message is the tagged-union envelope exchanged between the native
// host and the extension's service worker. Only fields relevant to
// Kind are populated; the rest serialize as omitempty.
//
// Host → extension kinds: "getCookies", "ping".
// Extension → host kinds: "ready", "cookies", "pong", "error".
type Message struct {
	ID        string       `json:"id,omitempty"`
	Kind      string       `json:"kind"`
	Domain    string       `json:"domain,omitempty"`
	Names     []string     `json:"names,omitempty"`
	UserAgent string       `json:"userAgent,omitempty"`
	Version   string       `json:"version,omitempty"`
	Error     string       `json:"error,omitempty"`
	Cookies   []WireCookie `json:"cookies,omitempty"`
}

// WireCookie mirrors chrome.cookies.Cookie's on-wire JSON shape.
// ExpirationDate is seconds-since-epoch as emitted by Chrome.
type WireCookie struct {
	Name           string  `json:"name"`
	Value          string  `json:"value"`
	Domain         string  `json:"domain"`
	Path           string  `json:"path,omitempty"`
	Secure         bool    `json:"secure,omitempty"`
	ExpirationDate float64 `json:"expirationDate,omitempty"`
	Session        bool    `json:"session,omitempty"`
}

// ToCookie converts the wire shape to the package's public Cookie.
func (w WireCookie) ToCookie() Cookie {
	c := Cookie{
		Domain: w.Domain,
		Name:   w.Name,
		Value:  w.Value,
		Path:   w.Path,
		Secure: w.Secure,
	}
	if !w.Session && w.ExpirationDate > 0 {
		sec := int64(w.ExpirationDate)
		nsec := int64((w.ExpirationDate - float64(sec)) * 1e9)
		c.Expires = time.Unix(sec, nsec).UTC()
	}
	return c
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
// for concurrent use; call it zero or more times per inbound message
// to push replies. Handler runs inside the ServeNativeHost loop, so
// it should not block on long operations — spawn a goroutine instead
// and use send from there.
type Handler func(ctx context.Context, in Message, send func(Message) error) error

// ServeNativeHost runs Chrome's stdin/stdout native-messaging loop
// against r/w, invoking handle for each inbound message. It returns
// nil on clean EOF (Chrome closed the port) and an error on framing
// or I/O failure.
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
			// Best-effort: report the decode error back, keep the loop
			// alive. A malformed frame shouldn't kill the host.
			_ = send(Message{Kind: "error", Error: err.Error()})
			continue
		}
		if err := handle(ctx, msg, send); err != nil {
			return err
		}
	}
}

// EchoHandler returns inbound messages verbatim with Kind="echo". Used
// by the native-host smoke-test stub; replaced by the real bridge in
// a later step once IPC is wired.
func EchoHandler() Handler {
	return func(ctx context.Context, in Message, send func(Message) error) error {
		out := in
		out.Kind = "echo"
		return send(out)
	}
}

// LogPath returns a sensible sidecar log path for the native host. We
// can't log to stdout — Chrome owns it. Parent directory is created
// on first call.
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
