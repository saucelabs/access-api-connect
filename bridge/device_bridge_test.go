package bridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// startEchoWSServer spins up a WebSocket server that records every binary
// frame it receives in the order they arrive. It does NOT respond on its
// own (tests can drive responses via the returned conn channel).
func startEchoWSServer(t *testing.T) (url string, frames *[][]byte, mu *sync.Mutex, accepted chan *websocket.Conn, cleanup func()) {
	t.Helper()
	var (
		gotFrames [][]byte
		gotMu     sync.Mutex
		upgrader  = websocket.Upgrader{}
	)
	connCh := make(chan *websocket.Conn, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
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

func TestDeviceBridgeAllocChannelIncrements(t *testing.T) {
	b := NewDeviceBridge("ws://x", "s", "u", "k")
	if got := b.AllocChannel(); got != 1 {
		t.Errorf("first AllocChannel = %d, want 1", got)
	}
	if got := b.AllocChannel(); got != 2 {
		t.Errorf("second AllocChannel = %d, want 2", got)
	}
}

func TestDeviceBridgeSendOpcodeProgression(t *testing.T) {
	url, frames, mu, accepted, cleanup := startEchoWSServer(t)
	defer cleanup()

	b := NewDeviceBridge(url, "session-id", "user", "key")
	if err := b.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer b.Close()

	// Wait for the server side to accept.
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("server never accepted the connection")
	}

	cid := b.AllocChannel()
	if err := b.SendToChannel(cid, []byte("first")); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if err := b.SendToChannel(cid, []byte("second")); err != nil {
		t.Fatalf("second send: %v", err)
	}
	if err := b.CloseChannel(cid); err != nil {
		t.Fatalf("CloseChannel: %v", err)
	}

	// Give the server a beat to receive both frames + the CLOSE.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(*frames)
		mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	got := *frames
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("captured %d frames, want 3", len(got))
	}

	// Frame #1 → OPEN with "first"
	op, gotCid, payload, err := decodeFrame(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if op != opOpen || gotCid != cid || string(payload) != "first" {
		t.Errorf("frame[0] = opcode=%d cid=%d payload=%q, want OPEN/%d/first", op, gotCid, payload, cid)
	}

	// Frame #2 → DATA with "second"
	op, gotCid, payload, err = decodeFrame(got[1])
	if err != nil {
		t.Fatal(err)
	}
	if op != opData || gotCid != cid || string(payload) != "second" {
		t.Errorf("frame[1] = opcode=%d cid=%d payload=%q, want DATA/%d/second", op, gotCid, payload, cid)
	}

	// Frame #3 → CLOSE with no payload
	op, gotCid, payload, err = decodeFrame(got[2])
	if err != nil {
		t.Fatal(err)
	}
	if op != opClose || gotCid != cid || len(payload) != 0 {
		t.Errorf("frame[2] = opcode=%d cid=%d payload=%q, want CLOSE/%d/<empty>", op, gotCid, payload, cid)
	}
}

func TestDeviceBridgeCloseChannelWithoutSendDoesNotEmitClose(t *testing.T) {
	url, frames, mu, accepted, cleanup := startEchoWSServer(t)
	defer cleanup()

	b := NewDeviceBridge(url, "s", "u", "k")
	if err := b.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer b.Close()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("server never accepted")
	}

	cid := b.AllocChannel()
	if err := b.CloseChannel(cid); err != nil {
		t.Fatalf("CloseChannel: %v", err)
	}

	// Give it a beat — no frame should arrive.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(*frames) != 0 {
		t.Errorf("expected 0 frames sent for never-opened channel, got %d", len(*frames))
	}
}
