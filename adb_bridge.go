package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

// adbConnSeq numbers accepted ADB connections for verbose logging.
var adbConnSeq atomic.Uint64

// AdbConnectionBridge runs once per accepted TCP connection on the ADB
// port. The Android endpoint carries raw ADB-protocol bytes inside binary
// WebSocket frames — no multiplex framing — so each local TCP connection
// gets its own dedicated WebSocket.
type AdbConnectionBridge struct {
	id        uint64
	wsURL     string
	sessionID string
	authB64   string
	conn      net.Conn
}

// NewAdbConnectionBridge constructs a bridge around the freshly accepted
// TCP conn. Call Run to drive it.
func NewAdbConnectionBridge(wsURL, sessionID, authB64 string, conn net.Conn) *AdbConnectionBridge {
	return &AdbConnectionBridge{
		id:        adbConnSeq.Add(1),
		wsURL:     wsURL,
		sessionID: sessionID,
		authB64:   authB64,
		conn:      conn,
	}
}

// Run dials the WebSocket and shovels bytes both directions until either
// side closes.
func (b *AdbConnectionBridge) Run(ctx context.Context) {
	peer := b.conn.RemoteAddr()
	vlog("adb client #%d connected: %s", b.id, peer)
	defer vlog("adb client #%d disconnected", b.id)
	defer b.conn.Close()

	header := http.Header{}
	header.Set("sessionId", b.sessionID)
	header.Set("Authorization", "Basic "+b.authB64)

	dialer := *websocket.DefaultDialer
	dialer.ReadBufferSize = 4 * 1024 * 1024
	dialer.WriteBufferSize = 4 * 1024 * 1024

	ws, _, err := dialer.DialContext(ctx, b.wsURL, header)
	if err != nil {
		log.Printf("failed to open WebSocket for adb client #%d (%s): %v", b.id, peer, err)
		return
	}

	var closeOnce sync.Once
	closeWS := func() { closeOnce.Do(func() { _ = ws.Close() }) }

	var wg sync.WaitGroup
	wg.Add(2)

	// TCP -> WS (raw ADB bytes, binary frames, no multiplex framing).
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
