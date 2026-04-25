// Command native-host is the Chrome native-messaging bridge for the
// Usage Buttons Stream Deck plugin. Chrome spawns this binary when the
// companion extension calls chrome.runtime.connectNative with the host
// name declared in the native-messaging host manifest.
//
// The host has two sides:
//
//   - stdin/stdout, framed per Chrome's native-messaging protocol,
//     talks to the extension's service worker.
//   - A TCP loopback listener (127.0.0.1:ephemeral, port published to
//     a sidecar file the plugin reads) talks to the Stream Deck plugin.
//
// The cookies.Bridge glues the two together: plugin cookie queries are
// forwarded to the extension, replies are correlated back by request
// ID. The bridge also tracks the handshake "ready" signal so the
// plugin's HostAvailable probe can distinguish "extension not yet
// connected" (quiet "waiting on browser" state) from "extension is up,
// cookies available."
//
// Logging must go to a sidecar file; writing to stdout would corrupt
// the frame stream.
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
)

func main() {
	if f, err := os.OpenFile(cookies.LogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		log.SetOutput(f)
		defer f.Close()
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.LUTC)
	cookies.LogSink = func(msg string) { log.Print(msg) }
	log.Printf("native-host: start pid=%d", os.Getpid())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	bridge := cookies.NewBridge()

	ln, err := cookies.ListenIPC()
	if err != nil {
		log.Printf("native-host: listen IPC: %v", err)
		os.Exit(1)
	}
	log.Printf("native-host: IPC listening on %s", cookies.IPCAddress())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		acceptLoop(ctx, ln, bridge)
	}()

	// Ping the extension every 20s so Chrome's SW idle timer (~30s)
	// never expires while we're attached. Without this the SW suspends,
	// Chrome kills us, and the plugin loses its bridge until the next
	// chrome.alarms wake — see commit fixing ext-version="" polling storms.
	keepaliveCtx, cancelKeepalive := context.WithCancel(ctx)
	defer cancelKeepalive()
	wg.Add(1)
	go func() {
		defer wg.Done()
		bridge.StartKeepalive(keepaliveCtx, 20*time.Second)
	}()

	err = cookies.ServeNativeHost(ctx, os.Stdin, os.Stdout, bridge.Handle)
	cancelKeepalive()
	log.Printf("native-host: extension port closed (err=%v)", err)
	bridge.OnExtensionDisconnect()
	_ = ln.Close()
	wg.Wait()

	switch {
	case err == nil, errors.Is(err, context.Canceled):
		log.Printf("native-host: clean exit")
	default:
		log.Printf("native-host: error: %v", err)
		os.Exit(1)
	}
}

func acceptLoop(ctx context.Context, ln net.Listener, bridge *cookies.Bridge) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Listener closed (normal on shutdown) — bail out silently.
			return
		}
		go bridge.HandlePluginConn(ctx, conn)
	}
}
