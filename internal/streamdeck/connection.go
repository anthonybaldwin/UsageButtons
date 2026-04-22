package streamdeck

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Connection wraps the WebSocket to the Stream Deck software.
type Connection struct {
	ws   *websocket.Conn
	mu   sync.Mutex // serialises writes
	uuid string
}

// ParseArgs extracts the registration arguments from os.Args.
// Stream Deck passes: -port P -pluginUUID U -registerEvent E -info {...}
func ParseArgs() RegistrationArgs {
	args := os.Args[1:]
	get := func(flag string) string {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	return RegistrationArgs{
		Port:          get("-port"),
		PluginUUID:    get("-pluginUUID"),
		RegisterEvent: get("-registerEvent"),
		Info:          json.RawMessage(get("-info")),
	}
}

// Connect establishes the WebSocket and sends the registration event.
func Connect(args RegistrationArgs) (*Connection, error) {
	url := fmt.Sprintf("ws://127.0.0.1:%s", args.Port)
	ctx := context.Background()
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", url, err)
	}
	ws.SetReadLimit(-1)

	c := &Connection{ws: ws, uuid: args.PluginUUID}

	// Register with Stream Deck.
	reg := struct {
		Event string `json:"event"`
		UUID  string `json:"uuid"`
	}{Event: args.RegisterEvent, UUID: args.PluginUUID}
	if err := c.sendJSON(reg); err != nil {
		ws.Close(websocket.StatusNormalClosure, "")
		return nil, fmt.Errorf("register: %w", err)
	}

	return c, nil
}

// ReadEvent blocks until the next inbound event arrives.
func (c *Connection) ReadEvent() (Event, error) {
	var ev Event
	err := wsjson.Read(context.Background(), c.ws, &ev)
	return ev, err
}

// Close closes the underlying WebSocket.
func (c *Connection) Close() error {
	return c.ws.Close(websocket.StatusNormalClosure, "")
}

// --- Outbound helpers ---

func (c *Connection) sendJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return wsjson.Write(context.Background(), c.ws, v)
}

// SetImage sends an SVG string as a base64 data URI to a key.
func (c *Connection) SetImage(context, svg string) {
	encoded := base64.StdEncoding.EncodeToString([]byte(svg))
	uri := "data:image/svg+xml;base64," + encoded
	if err := c.sendJSON(SetImageEvent{
		Event:   "setImage",
		Context: context,
		Payload: SetImagePayload{Image: uri, Target: 0},
	}); err != nil {
		log.Printf("[streamdeck] setImage error: %v", err)
	}
}

// SetTitle sets the native Stream Deck title for a key.
func (c *Connection) SetTitle(ctx, title string) {
	if err := c.sendJSON(SetTitleEvent{
		Event:   "setTitle",
		Context: ctx,
		Payload: SetTitlePayload{Title: title, Target: 0},
	}); err != nil {
		log.Printf("[streamdeck] setTitle error: %v", err)
	}
}

// OpenURL opens a URL in the user's default browser.
func (c *Connection) OpenURL(url string) {
	if err := c.sendJSON(OpenURLEvent{
		Event:   "openUrl",
		Payload: OpenURLPayload{URL: url},
	}); err != nil {
		log.Printf("[streamdeck] openUrl error: %v", err)
	}
}

// GetGlobalSettings requests the plugin-wide settings.
func (c *Connection) GetGlobalSettings() {
	c.sendJSON(SimpleEvent{Event: "getGlobalSettings", Context: c.uuid})
}

// SetSettings persists per-key settings.
func (c *Connection) SetSettings(context string, settings json.RawMessage) {
	c.sendJSON(SetSettingsEvent{
		Event:   "setSettings",
		Context: context,
		Payload: settings,
	})
}

// SetGlobalSettings persists plugin-wide settings.
func (c *Connection) SetGlobalSettings(settings json.RawMessage) {
	c.sendJSON(GlobalSettingsEvent{
		Event:   "setGlobalSettings",
		Context: c.uuid,
		Payload: settings,
	})
}

// SendToPropertyInspector delivers a custom JSON payload to the PI
// for the given action + context. Used to reply to custom
// sendToPlugin requests (e.g., cookie-host status, registration
// results).
func (c *Connection) SendToPropertyInspector(context, action string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[streamdeck] sendToPropertyInspector marshal: %v", err)
		return
	}
	if err := c.sendJSON(SendToPropertyInspectorEvent{
		Event:   "sendToPropertyInspector",
		Action:  action,
		Context: context,
		Payload: raw,
	}); err != nil {
		log.Printf("[streamdeck] sendToPropertyInspector error: %v", err)
	}
}

// Log writes a message to Stream Deck's per-plugin log file.
func (c *Connection) Log(msg string) {
	c.sendJSON(LogMessageEvent{
		Event:   "logMessage",
		Payload: LogMessagePayload{Message: msg},
	})
}

// Logf is a formatted version of Log.
func (c *Connection) Logf(format string, args ...any) {
	c.Log(fmt.Sprintf(format, args...))
}

// ProviderIDFromAction extracts the provider ID from a Stream Deck
// action UUID. Returns "" if the UUID doesn't belong to this plugin.
//
// e.g. "io.github.anthonybaldwin.usagebuttons.claude" → "claude"
func ProviderIDFromAction(action string) string {
	const prefix = "io.github.anthonybaldwin.usagebuttons."
	lower := strings.ToLower(action)
	if !strings.HasPrefix(lower, prefix) {
		return ""
	}
	id := lower[len(prefix):]
	if id == "" {
		return ""
	}
	return id
}
