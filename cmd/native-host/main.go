// Command native-host is the Chrome native-messaging bridge for the
// Usage Buttons Stream Deck plugin. Chrome spawns this binary when the
// companion extension calls chrome.runtime.connectNative with the host
// name declared in the native-messaging host manifest.
//
// Stdout and stdin are framed per Chrome's native-messaging protocol
// (4-byte LE length + JSON payload). Logging must go to a sidecar
// file; writing to stdout would corrupt the frame stream.
//
// Current scope: echo handler. Later steps add an IPC server (named
// pipe on Windows, Unix socket on macOS) that lets the plugin dial in
// and request cookies via this host.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"

	"github.com/anthonybaldwin/UsageButtons/internal/cookies"
)

func main() {
	f, err := os.OpenFile(cookies.LogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		log.SetOutput(f)
		defer f.Close()
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.LUTC)
	log.Printf("native-host: start pid=%d args=%v", os.Getpid(), os.Args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	err = cookies.ServeNativeHost(ctx, os.Stdin, os.Stdout, cookies.EchoHandler())
	switch {
	case err == nil:
		log.Printf("native-host: clean exit (stdin EOF)")
	case errors.Is(err, context.Canceled):
		log.Printf("native-host: shutdown via signal")
	default:
		log.Printf("native-host: error: %v", err)
		os.Exit(1)
	}
}
