package bridge

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"net"
	"sync"
	"time"
)

// Both serve functions take a net.Listener rather than an address, so the
// caller decides how the device is exposed locally. The CLI listens on
// /var/run/usbmuxd (unix, root) for iOS and 127.0.0.1:7001 (tcp) for
// Android; a test can listen on a loopback TCP port instead — go-ios reads
// USBMUXD_SOCKET_ADDRESS as tcp://host:port when it contains a colon, so
// serving usbmux over TCP needs no root and takes over no system socket.

// verboseLogging gates the per-connection log lines. Set it once via
// SetVerboseLogging before any serving starts, so the serving goroutines can
// read it without a lock thereafter.
var verboseLogging bool

// SetVerboseLogging enables per-connection log lines. Call it before any
// Serve function; it is not safe to change once connections are being served.
func SetVerboseLogging(enabled bool) {
	verboseLogging = enabled
}

func vlog(format string, args ...interface{}) {
	if verboseLogging {
		log.Printf(format, args...)
	}
}

// BasicAuthHeader builds an HTTP Basic Authorization header value from raw
// Sauce credentials.
func BasicAuthHeader(username, accessKey string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+accessKey))
}

// ServeADB accepts connections on l and gives each one its own WebSocket
// carrying raw ADB-protocol bytes. Point an adb server at l's address with
// `adb connect <addr>` to attach.
//
// Blocks until l fails or ctx is cancelled, then waits for the in-flight
// connections to finish.
func ServeADB(ctx context.Context, l net.Listener, wsURL, sessionID, authB64 string) error {
	defer l.Close()
	log.Printf("listening on %s", l.Addr())

	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return err
		}
		connBridge := NewAdbConnectionBridge(wsURL, sessionID, authB64, conn)
		wg.Add(1)
		go func() {
			defer wg.Done()
			connBridge.Run(ctx)
		}()
	}
}

// ServeUsbmux accepts connections on l and services each one against the
// shared multiplexed bridge, answering the cheap usbmux requests locally and
// forwarding the rest upstream on their own channel.
//
// The bridge's reader loop (RunUntilError) must already be running.
func ServeUsbmux(ctx context.Context, l net.Listener, b LocalBridge) error {
	defer l.Close()
	log.Printf("listening on %s", l.Addr())

	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return err
		}
		go NewLocalUsbmuxHandler(b, conn).Run()
	}
}

// ReconnectWithBackoff redials the session WebSocket with capped backoff
// (0.5s-5s). Each attempt first reads the session state: no longer ACTIVE
// ends the loop with an error (caller exits); a failed read (unreachable API,
// HTTP error, bad body) is transient and retried. On success the reader is
// restarted and a device-properties round trip confirms the tunnel.
//
// readerErrCh is the channel the caller runs RunUntilError against; this
// function restarts that goroutine on every successful redial.
func ReconnectWithBackoff(ctx context.Context, b *DeviceBridge, apiURL, sessionID, authHeader string, readerErrCh chan error) error {
	const maxBackoff = 5 * time.Second
	backoff := 500 * time.Millisecond

	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Non-ACTIVE stops the loop; a failed read (err != nil) is transient.
		if info, err := FetchSession(ctx, apiURL, sessionID, authHeader); err == nil {
			if state, _ := info["state"].(string); state != "ACTIVE" {
				return errors.New("session is no longer active (state=" + state + ")")
			}
		}

		if err := b.Reconnect(ctx); err != nil {
			log.Printf("reconnect attempt %d failed: %v (next try in %s)", attempt, err, backoff)
		} else {
			go func() { readerErrCh <- b.RunUntilError(ctx) }()
			if _, err := b.FetchDeviceProperties(ctx, 10*time.Second); err == nil {
				return nil
			} else {
				log.Printf("reconnect attempt %d: tunnel check failed: %v (next try in %s)", attempt, err, backoff)
				_ = b.Close()
				<-readerErrCh // reap the reader we just started
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}
