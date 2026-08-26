package bridge

import (
	"context"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

// rawConnSeq numbers accepted connections for verbose logging.
var rawConnSeq atomic.Uint64

// RawConnectionBridge runs once per accepted connection and copies bytes
// between it and a WebSocket without interpreting them. It suits the endpoints
// that carry a protocol unwrapped — adbUrl on Android, usbmuxdUrl on iOS —
// where there is no multiplex framing, so each local connection gets its own
// dedicated WebSocket.
type RawConnectionBridge struct {
	id        uint64
	wsURL     string
	sessionID string
	authB64   string
	conn      net.Conn
}

// NewRawConnectionBridge constructs a bridge around the freshly accepted
// conn. Call Run to drive it.
func NewRawConnectionBridge(wsURL, sessionID, authB64 string, conn net.Conn) *RawConnectionBridge {
	return &RawConnectionBridge{
		id:        rawConnSeq.Add(1),
		wsURL:     wsURL,
		sessionID: sessionID,
		authB64:   authB64,
		conn:      conn,
	}
}

// Run dials the WebSocket and shovels bytes both directions until either
// side closes.
func (b *RawConnectionBridge) Run(ctx context.Context) {
	peer := b.conn.RemoteAddr()
	vlog("client #%d connected: %s", b.id, peer)
	defer vlog("client #%d disconnected", b.id)
	defer b.conn.Close()

	header := http.Header{}
	header.Set("sessionId", b.sessionID)
	header.Set("Authorization", "Basic "+b.authB64)

	dialer := *websocket.DefaultDialer
	dialer.ReadBufferSize = 4 * 1024 * 1024
	dialer.WriteBufferSize = 4 * 1024 * 1024

	ws, _, err := dialer.DialContext(ctx, b.wsURL, header)
	if err != nil {
		log.Printf("failed to open WebSocket for client #%d (%s): %v", b.id, peer, err)
		return
	}

	var closeOnce sync.Once
	closeWS := func() { closeOnce.Do(func() { _ = ws.Close() }) }

	var wg sync.WaitGroup
	wg.Add(2)

	// Local -> WS (binary frames, no multiplex framing, no interpretation).
	go func() {
		defer wg.Done()
		defer closeWS()
		buf := make([]byte, 65536)
		for {
			n, err := b.conn.Read(buf)
			if err != nil {
				return
			}
			if err := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				return
			}
		}
	}()

	// WS -> TCP.
	go func() {
		defer wg.Done()
		defer b.conn.Close()
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.BinaryMessage {
				log.Printf("ignoring non-binary frame from server")
				continue
			}
			if _, err := b.conn.Write(data); err != nil {
				return
			}
		}
	}()

	wg.Wait()
}
