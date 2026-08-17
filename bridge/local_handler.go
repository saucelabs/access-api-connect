package bridge

import (
	"log"
	"net"
	"sync"
	"sync/atomic"
)

// LocalBridge is the subset of DeviceBridge the local usbmuxd handler
// actually needs. Defining it as an interface lets tests substitute a
// fake without dragging in a real WebSocket.
type LocalBridge interface {
	AllocChannel() uint32
	RegisterHandlers(cid uint32, onData DataHandler, onClose CloseHandler)
	SendToChannel(cid uint32, payload []byte) error
	CloseChannel(cid uint32) error
	DeviceProperties() map[string]interface{}
	LocalDeviceID() int
}

// localConnSeq gives each accepted /var/run/usbmuxd connection a tiny,
// human-readable id. Unix-domain peer addresses are always empty, so we
// use this counter instead — connect/disconnect lines stay correlatable.
var localConnSeq atomic.Uint64

// LocalUsbmuxHandler runs once per accepted connection on /var/run/usbmuxd.
//
// Replies locally to ListDevices / Listen / SavePairRecord (cheap; avoids
// round trips), and forwards Connect / ReadPairRecord / ReadBUID upstream
// over a fresh multiplex channel. After the trigger PLIST is forwarded,
// the local socket switches to raw passthrough on that channel.
type LocalUsbmuxHandler struct {
	id              uint64
	bridge          LocalBridge
	conn            net.Conn
	upstreamChannel uint32
	inPassthrough   bool
	buf             []byte
	writeMu         sync.Mutex
	closed          atomic.Bool
}

// NewLocalUsbmuxHandler wires the handler to a freshly accepted conn.
func NewLocalUsbmuxHandler(bridge LocalBridge, conn net.Conn) *LocalUsbmuxHandler {
	return &LocalUsbmuxHandler{
		id:     localConnSeq.Add(1),
		bridge: bridge,
		conn:   conn,
	}
}

// Run drives the local-side state machine until the conn closes.
func (h *LocalUsbmuxHandler) Run() {
	vlog("local client #%d connected", h.id)
	defer vlog("local client #%d disconnected", h.id)
	defer h.close()

	chunk := make([]byte, 65536)
	for {
		n, err := h.conn.Read(chunk)
		if err != nil {
			return
		}
		data := chunk[:n]

		if h.inPassthrough {
			if err := h.bridge.SendToChannel(h.upstreamChannel, data); err != nil {
				return
			}
			continue
		}

		h.buf = append(h.buf, data...)
		// Drain any complete PLIST messages we have buffered.
		for {
			msg, rest, ok, err := tryDecodeUsbmuxMessage(h.buf)
			if err != nil {
				log.Printf("usbmux decode error: %v", err)
				return
			}
			if !ok {
				break
			}
			h.buf = rest
			if err := h.handleLocalMessage(msg); err != nil {
				log.Printf("local handler error: %v", err)
				return
			}
			if h.inPassthrough {
				// Switched to passthrough mid-loop. Anything still in
				// h.buf is raw payload that belongs on the upstream channel.
				if len(h.buf) > 0 {
					_ = h.bridge.SendToChannel(h.upstreamChannel, h.buf)
					h.buf = nil
				}
				break
			}
		}
	}
}

func (h *LocalUsbmuxHandler) close() {
	if !h.closed.CompareAndSwap(false, true) {
		return
	}
	if h.inPassthrough {
		_ = h.bridge.CloseChannel(h.upstreamChannel)
	}
	_ = h.conn.Close()
}

func (h *LocalUsbmuxHandler) writeLocal(data []byte) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	_, err := h.conn.Write(data)
	return err
}

func (h *LocalUsbmuxHandler) respondResult(version, type_, tag uint32, number int) error {
	body, err := encodeUsbmuxMessage(version, type_, tag, map[string]interface{}{
		"MessageType": "Result",
		"Number":      number,
	})
	if err != nil {
		return err
	}
	return h.writeLocal(body)
}

func (h *LocalUsbmuxHandler) deviceDictionary() map[string]interface{} {
	props := h.bridge.DeviceProperties()
	if props == nil {
		return nil
	}
	deviceID := h.bridge.LocalDeviceID()
	// Override DeviceID with our local id so the local client always
	// sees a stable identifier.
	props["DeviceID"] = deviceID
	return map[string]interface{}{
		"MessageType": "Attached",
		"DeviceID":    deviceID,
		"Properties":  props,
	}
}

func (h *LocalUsbmuxHandler) respondListDevices(version, type_, tag uint32) error {
	deviceList := []interface{}{}
	if device := h.deviceDictionary(); device != nil {
		deviceList = append(deviceList, device)
	}
	body, err := encodeUsbmuxMessage(version, type_, tag, map[string]interface{}{
		"DeviceList": deviceList,
	})
	if err != nil {
		return err
	}
	return h.writeLocal(body)
}

func (h *LocalUsbmuxHandler) sendAttached(version, type_ uint32) error {
	device := h.deviceDictionary()
	if device == nil {
		return nil
	}
	// Attached notifications use tag 0.
	body, err := encodeUsbmuxMessage(version, type_, 0, device)
	if err != nil {
		return err
	}
	return h.writeLocal(body)
}

func (h *LocalUsbmuxHandler) startPassthrough(version, type_, tag uint32, payload map[string]interface{}) error {
	cid := h.bridge.AllocChannel()
	h.upstreamChannel = cid
	h.inPassthrough = true

	onData := func(data []byte) error {
		if err := h.writeLocal(data); err != nil {
			_ = h.bridge.CloseChannel(cid)
			return err
		}
		return nil
	}
	onClose := func() error {
		_ = h.conn.Close()
		return nil
	}
	h.bridge.RegisterHandlers(cid, onData, onClose)

	// Re-encode and forward the PLIST that triggered the switch.
	raw, err := encodeUsbmuxMessage(version, type_, tag, payload)
	if err != nil {
		return err
	}
	return h.bridge.SendToChannel(cid, raw)
}

func (h *LocalUsbmuxHandler) handleLocalMessage(msg *usbmuxMessage) error {
	messageType, _ := msg.Payload["MessageType"].(string)
	switch messageType {
	case "ListDevices":
		return h.respondListDevices(msg.Version, msg.Type, msg.Tag)
	case "Listen":
		if err := h.respondResult(msg.Version, msg.Type, msg.Tag, 0); err != nil {
			return err
		}
		return h.sendAttached(msg.Version, msg.Type)
	case "SavePairRecord":
		return h.respondResult(msg.Version, msg.Type, msg.Tag, 0)
	case "ReadPairRecord", "ReadBUID", "Connect":
		return h.startPassthrough(msg.Version, msg.Type, msg.Tag, msg.Payload)
	default:
		log.Printf("unhandled local MessageType=%q", messageType)
		return h.respondResult(msg.Version, msg.Type, msg.Tag, 1)
	}
}
