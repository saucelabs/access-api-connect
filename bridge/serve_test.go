package bridge

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// listenLocal binds a loopback TCP port and hands back the listener. Binding
// a high loopback port needs no privileges, which is the point of the tests
// below: the CLI serves usbmux on /var/run/usbmuxd and therefore needs root,
// while an importing program can serve the same protocol on TCP and not.
func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}
	return listener
}

// readUsbmuxMessage accumulates from conn until one complete PLIST message
// has arrived, so the assertions do not depend on how the reply is segmented.
func readUsbmuxMessage(t *testing.T, conn net.Conn, timeout time.Duration) *usbmuxMessage {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))

	var buf []byte
	chunk := make([]byte, 4096)
	for {
		n, err := conn.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			msg, _, ok, decodeErr := tryDecodeUsbmuxMessage(buf)
			if decodeErr != nil {
				t.Fatalf("decode usbmux reply: %v", decodeErr)
			}
			if ok {
				return msg
			}
		}
		if err != nil {
			t.Fatalf("read usbmux reply (%d bytes buffered): %v", len(buf), err)
		}
	}
}

// waitForFrameCount blocks until the recording WebSocket server has seen at
// least want frames, and returns a copy of them.
func waitForFrameCount(t *testing.T, frames *[][]byte, mu interface {
	Lock()
	Unlock()
}, want int, timeout time.Duration) [][]byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		mu.Lock()
		got := append([][]byte(nil), *frames...)
		mu.Unlock()
		if len(got) >= want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d frames reached the websocket within %s", len(got), want, timeout)
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestServeUsbmuxOverTCP is the test that matters for reusing this package:
// the local usbmux protocol works over a plain TCP listener, so nothing has
// to take over /var/run/usbmuxd and nothing needs root. go-ios reads
// USBMUXD_SOCKET_ADDRESS as tcp://host:port when it contains a colon, so this
// address can be handed straight to it.
func TestServeUsbmuxOverTCP(t *testing.T) {
	br := newFakeBridge(map[string]interface{}{"SerialNumber": "abc123", "ProductType": "iPhone15,3"}, 1)
	listener := listenLocal(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() { serveErr <- ServeUsbmux(ctx, listener, br) }()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial the tcp usbmux socket: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write(encodePlist(t, "ListDevices", nil)); err != nil {
		t.Fatalf("write ListDevices: %v", err)
	}

	msg := readUsbmuxMessage(t, conn, 2*time.Second)
	devices, _ := msg.Payload["DeviceList"].([]interface{})
	if len(devices) != 1 {
		t.Fatalf("DeviceList len = %d, want 1", len(devices))
	}
	device, _ := devices[0].(map[string]interface{})
	props, _ := device["Properties"].(map[string]interface{})
	if serial, _ := props["SerialNumber"].(string); serial != "abc123" {
		t.Errorf("SerialNumber = %q, want %q", serial, "abc123")
	}

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Errorf("ServeUsbmux returned %v, want nil after cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeUsbmux did not return after cancel")
	}
}

// TestServeADBOverTCPForwardsBothDirections checks the Android path end to
// end through the exported API: bytes written to the local port arrive on the
// WebSocket as a binary frame, and frames sent back arrive on the local port.
func TestServeADBOverTCPForwardsBothDirections(t *testing.T) {
	wsURL, frames, mu, accepted, cleanup := startEchoWSServer(t)
	defer cleanup()

	listener := listenLocal(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() { serveErr <- ServeADB(ctx, listener, wsURL, "session-1", "dXNlcjprZXk=") }()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial the tcp adb socket: %v", err)
	}

	// Recognisable adb-ish bytes; the bridge is protocol-agnostic here.
	outbound := []byte("CNXN\x00\x00\x00\x01")
	if _, err := conn.Write(outbound); err != nil {
		t.Fatalf("write to the local port: %v", err)
	}

	got := waitForFrameCount(t, frames, mu, 1, 2*time.Second)
	if !bytes.Equal(got[0], outbound) {
		t.Errorf("websocket received %q, want %q", got[0], outbound)
	}

	ws := <-accepted
	inbound := []byte("OKAY")
	if err := ws.WriteMessage(websocket.BinaryMessage, inbound); err != nil {
		t.Fatalf("write back over the websocket: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read the reply on the local port: %v", err)
	}
	if !bytes.Equal(buf[:n], inbound) {
		t.Errorf("local port received %q, want %q", buf[:n], inbound)
	}

	// Close the local conn before cancelling: the copy loops end on socket
	// closure, and ServeADB waits for its connection goroutines on the way out.
	_ = conn.Close()
	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Errorf("ServeADB returned %v, want nil after cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeADB did not return after cancel")
	}
}

// TestServeADBReturnsNilOnContextCancel pins the shutdown contract an
// importing test depends on: cancelling the context is a clean stop, not an
// error to be distinguished from a real listener failure.
func TestServeADBReturnsNilOnContextCancel(t *testing.T) {
	listener := listenLocal(t)

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- ServeADB(ctx, listener, "ws://unused.invalid", "session-1", "dXNlcjprZXk=") }()

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Errorf("ServeADB returned %v, want nil after cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeADB did not return after cancel")
	}
}
