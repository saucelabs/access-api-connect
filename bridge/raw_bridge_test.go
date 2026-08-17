package bridge

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// startEchoServer spins up a binary-echo WebSocket server. Every
// inbound binary frame is captured AND echoed back prefixed with "ECHO:".
func startEchoServer(t *testing.T) (url string, captured *[][]byte, mu *sync.Mutex, cleanup func()) {
	t.Helper()
	var (
		got      [][]byte
		gotMu    sync.Mutex
		upgrader = websocket.Upgrader{}
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer ws.Close()
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.BinaryMessage {
				continue
			}
			gotMu.Lock()
			got = append(got, append([]byte(nil), data...))
			gotMu.Unlock()
			if err := ws.WriteMessage(websocket.BinaryMessage, append([]byte("ECHO:"), data...)); err != nil {
				return
			}
		}
	}))

	url = "ws" + strings.TrimPrefix(srv.URL, "http")
	return url, &got, &gotMu, srv.Close
}

func TestRawConnectionBridgePassthrough(t *testing.T) {
	wsURL, captured, mu, cleanup := startEchoServer(t)
	defer cleanup()

	// Stand up a TCP listener that hands each conn to RawConnectionBridge.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		NewRawConnectionBridge(wsURL, "session", "Yg==", conn).Run(ctx)
	}()

	// Connect as if we were `adb` doing CNXN.
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	send := []byte("CNXN\x00\x00\x00\x01host::features=shell")
	if _, err := client.Write(send); err != nil {
		t.Fatal(err)
	}

	// Wait for echo back.
	buf := make([]byte, 128)
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	got := buf[:n]
	if !bytes.HasPrefix(got, []byte("ECHO:CNXN")) {
		t.Errorf("client got %q, want ECHO:CNXN…", got)
	}

	// Verify the server received the raw bytes without any 0xBEEF framing
	// in front of them — the whole point of the Android path.
	deadline := time.Now().Add(1 * time.Second)
	for {
		mu.Lock()
		n := len(*captured)
		mu.Unlock()
		if n >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*captured) == 0 {
		t.Fatal("no frame captured by WS server")
	}
	first := (*captured)[0]
	if len(first) >= 4 && bytes.Equal(first[:4], []byte{0x00, 0x00, 0xBE, 0xEF}) {
		t.Errorf("expected raw ADB bytes, but frame still carries 0xBEEF multiplex magic: %x", first[:8])
	}
	if !bytes.HasPrefix(first, []byte("CNXN")) {
		t.Errorf("first frame doesn't start with CNXN: %x", first[:min(8, len(first))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
