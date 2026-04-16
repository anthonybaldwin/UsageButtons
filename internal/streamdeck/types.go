// Package streamdeck implements the Stream Deck SDK v2 WebSocket protocol.
package streamdeck

import "encoding/json"

// RegistrationArgs are parsed from the command-line flags that the
// Stream Deck software passes when launching a plugin binary.
type RegistrationArgs struct {
	Port          string
	PluginUUID    string
	RegisterEvent string
	Info          json.RawMessage
}

// --- Inbound events (Stream Deck → plugin) ---

// Event is the minimal envelope for every inbound message.
type Event struct {
	Event   string          `json:"event"`
	Action  string          `json:"action,omitempty"`
	Context string          `json:"context,omitempty"`
	Device  string          `json:"device,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// WillAppearPayload is the payload of a willAppear event.
type WillAppearPayload struct {
	Settings  json.RawMessage `json:"settings"`
	Row       int             `json:"row"`
	Column    int             `json:"column"`
	IsInMulti bool            `json:"isInMultiAction"`
}

// DidReceiveSettingsPayload is the payload of didReceiveSettings.
type DidReceiveSettingsPayload struct {
	Settings json.RawMessage `json:"settings"`
}

// GlobalSettingsPayload wraps didReceiveGlobalSettings.
type GlobalSettingsPayload struct {
	Settings json.RawMessage `json:"settings"`
}

// SendToPluginPayload is a custom PI → plugin event.
type SendToPluginPayload struct {
	Action string `json:"action,omitempty"`
}

// --- Outbound events (plugin → Stream Deck) ---

// SetImageEvent sets the button image (SVG data URI).
type SetImageEvent struct {
	Event   string         `json:"event"`
	Context string         `json:"context"`
	Payload SetImagePayload `json:"payload"`
}

// SetImagePayload carries the data URI.
type SetImagePayload struct {
	Image  string `json:"image"`
	Target int    `json:"target"`
}

// SetTitleEvent sets the button title.
type SetTitleEvent struct {
	Event   string         `json:"event"`
	Context string         `json:"context"`
	Payload SetTitlePayload `json:"payload"`
}

// SetTitlePayload carries the title text.
type SetTitlePayload struct {
	Title  string `json:"title"`
	Target int    `json:"target"`
}

// SetSettingsEvent persists per-key settings.
type SetSettingsEvent struct {
	Event   string          `json:"event"`
	Context string          `json:"context"`
	Payload json.RawMessage `json:"payload"`
}

// OpenURLEvent opens a URL in the default browser.
type OpenURLEvent struct {
	Event   string          `json:"event"`
	Payload OpenURLPayload  `json:"payload"`
}

// OpenURLPayload carries the URL to open.
type OpenURLPayload struct {
	URL string `json:"url"`
}

// LogMessageEvent writes to Stream Deck's plugin log.
type LogMessageEvent struct {
	Event   string           `json:"event"`
	Payload LogMessagePayload `json:"payload"`
}

// LogMessagePayload carries the log text.
type LogMessagePayload struct {
	Message string `json:"message"`
}

// SimpleEvent is for events with just event + context (getSettings,
// getGlobalSettings, showAlert, showOk).
type SimpleEvent struct {
	Event   string `json:"event"`
	Context string `json:"context"`
}

// GlobalSettingsEvent sets plugin-wide global settings.
type GlobalSettingsEvent struct {
	Event   string          `json:"event"`
	Context string          `json:"context"`
	Payload json.RawMessage `json:"payload"`
}

// SendToPropertyInspectorEvent delivers a custom payload from the
// plugin back to the Property Inspector. Action is the action UUID of
// the key whose PI is open (taken from the inbound sendToPlugin
// event).
type SendToPropertyInspectorEvent struct {
	Event   string          `json:"event"`
	Action  string          `json:"action"`
	Context string          `json:"context"`
	Payload json.RawMessage `json:"payload"`
}
