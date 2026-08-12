package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// startDroppableWSServer is like startEchoWSServer but supports multiple
// sequential connections and exposes the raw net.Conn of each accept so a
// test can kill it abruptly (no close handshake), mimicking a mid-path
// reset or NAT expiry.
func startDroppableWSServer(t *testing.T) (url string, frames *[][]byte, mu *sync.Mutex, accepted chan *websocket.Conn, cleanup func()) {
	t.Helper()
	var (
		gotFrames [][]byte
		gotMu     sync.Mutex
		upgrader  = websocket.Upgrader{}
	)
	connCh := make(chan *websocket.Conn, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return // second Upgrade on a dead test can legitimately fail
		}
		connCh <- ws
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			gotMu.Lock()
			gotFrames = append(gotFrames, append([]byte(nil), data...))
			gotMu.Unlock()
		}
	}))

	url = "ws" + strings.TrimPrefix(srv.URL, "http")
	return url, &gotFrames, &gotMu, connCh, srv.Close
}

func waitForFrames(t *testing.T, frames *[][]byte, mu *sync.Mutex, want int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(*frames)
		mu.Unlock()
		if n >= want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d frames, have %d", want, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([][]byte(nil), *frames...)
}

// TestBridgeReconnectAfterServerDrop simulates the incident end to end:
// the connection dies without a close handshake, channel state is
// invalidated (close handlers fire), and after Reconnect a new channel
// works over the fresh connection, starting with OPEN.
func TestBridgeReconnectAfterServerDrop(t *testing.T) {
	url, frames, mu, accepted, cleanup := startDroppableWSServer(t)
	defer cleanup()

	b := NewDeviceBridge(url, "session-id", "user", "key")
	if err := b.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer b.Close()

	readerErr := make(chan error, 1)
	go func() { readerErr <- b.RunUntilError(context.Background()) }()

	var serverConn *websocket.Conn
	select {
	case serverConn = <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("server never accepted connection #1")
	}

	// A live channel with a close handler, standing in for a passthrough
	// usbmux connection.
	cid1 := b.AllocChannel()
	var closed atomic.Bool
	b.RegisterHandlers(cid1, func([]byte) error { return nil }, func() error {
		closed.Store(true)
		return nil
	})
	if err := b.SendToChannel(cid1, []byte("hello")); err != nil {
		t.Fatalf("send on channel 1: %v", err)
	}
	waitForFrames(t, frames, mu, 1)

	// Kill the connection abruptly — no WebSocket close handshake.
	if err := serverConn.UnderlyingConn().(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatalf("SetLinger: %v", err)
	}
	_ = serverConn.UnderlyingConn().Close()

	select {
	case err := <-readerErr:
		if err == nil {
			t.Fatal("reader returned nil error after connection drop")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reader did not notice the dropped connection")
	}

	// Invalidate old channels: the close handler must fire, and sends on
	// the stale channel must fail rather than emit frames.
	b.ResetForReconnect()
	if !closed.Load() {
		t.Error("close handler did not fire on ResetForReconnect")
	}
	if err := b.SendToChannel(cid1, []byte("stale")); err == nil {
		t.Error("send on stale channel succeeded, want error")
	}

	// Redial and prove a fresh channel works, starting with OPEN.
	if err := b.Reconnect(context.Background()); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	go func() { readerErr <- b.RunUntilError(context.Background()) }()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("server never accepted connection #2")
	}

	cid2 := b.AllocChannel()
	if cid2 <= cid1 {
		t.Errorf("channel id not monotonic across reconnect: %d after %d", cid2, cid1)
	}
	if err := b.SendToChannel(cid2, []byte("again")); err != nil {
		t.Fatalf("send after reconnect: %v", err)
	}
	got := waitForFrames(t, frames, mu, 2)

	op, gotCid, payload, err := decodeFrame(got[1])
	if err != nil {
		t.Fatal(err)
	}
	if op != opOpen || gotCid != cid2 || string(payload) != "again" {
		t.Errorf("post-reconnect frame = opcode=%d cid=%d payload=%q, want OPEN/%d/again", op, gotCid, payload, cid2)
	}
}

// TestBridgeDetectsSilentlyDeadConnection covers the sleep/blackhole
// scenario: the server stops answering pings (and sends nothing), and the
// client's pong deadline must fail the read quickly — instead of blocking
// until the OS TCP timeout minutes later.
func TestBridgeDetectsSilentlyDeadConnection(t *testing.T) {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Swallow pings instead of pong-ing (a blackholed path looks the
		// same to the client), and read so control frames get processed.
		ws.SetPingHandler(func(string) error { return nil })
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	b := NewDeviceBridge("ws"+strings.TrimPrefix(srv.URL, "http"), "s", "u", "k")
	b.pingInterval, b.pongTimeout = 50*time.Millisecond, 250*time.Millisecond
	if err := b.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer b.Close()

	start := time.Now()
	err := b.RunUntilError(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("RunUntilError returned nil, want deadline error")
	}
	var nerr net.Error
	if !errors.As(err, &nerr) || !nerr.Timeout() {
		t.Errorf("error = %v, want a net timeout error", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("dead connection detected after %s, want ~pongTimeout (250ms)", elapsed)
	}
}

// TestBridgeAnswersServerPings makes sure the custom ping handler still
// replies with a pong — the server drops sessions whose pongs stop, so
// this behavior must survive the keepalive changes.
func TestBridgeAnswersServerPings(t *testing.T) {
	upgrader := websocket.Upgrader{}
	gotPong := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		ws.SetPongHandler(func(string) error {
			select {
			case gotPong <- struct{}{}:
			default:
			}
			return nil
		})
		if err := ws.WriteControl(websocket.PingMessage, []byte("ka"), time.Now().Add(time.Second)); err != nil {
			return
		}
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	b := NewDeviceBridge("ws"+strings.TrimPrefix(srv.URL, "http"), "s", "u", "k")
	if err := b.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer b.Close()

	done := make(chan struct{})
	go func() { _ = b.RunUntilError(context.Background()); close(done) }()

	select {
	case <-gotPong:
	case <-time.After(2 * time.Second):
		t.Fatal("server never received a pong for its ping")
	}
	b.Close()
	<-done
}
