package bridge

import (
	"encoding/binary"
	"fmt"
)

// Multiplex framing used by the multiplex-capable WebSocket endpoint.
//
// Every binary frame on the WebSocket starts with a 10-byte header:
//
//	| 4-byte magic 0xBEEF | 2-byte opcode | 4-byte channel-id | payload |
//
// Opcodes:
//
//	1 = OPEN  — first frame on a new channel
//	2 = DATA  — every subsequent frame on the channel
//	3 = CLOSE — channel teardown (either direction)

const (
	magic     = 0xBEEF
	opOpen    = 1
	opData    = 2
	opClose   = 3
	headerLen = 4 + 2 + 4 // magic + opcode + channel-id
)

// encodeFrame builds the 10-byte multiplex header in front of payload.
func encodeFrame(opcode uint16, channelID uint32, payload []byte) []byte {
	buf := make([]byte, headerLen+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], magic)
	binary.BigEndian.PutUint16(buf[4:6], opcode)
	binary.BigEndian.PutUint32(buf[6:10], channelID)
	copy(buf[headerLen:], payload)
	return buf
}

// decodeFrame is the inverse of encodeFrame.
func decodeFrame(buf []byte) (opcode uint16, channelID uint32, payload []byte, err error) {
	if len(buf) < headerLen {
		return 0, 0, nil, fmt.Errorf("frame too short (%d bytes)", len(buf))
	}
	if m := binary.BigEndian.Uint32(buf[0:4]); m != magic {
		return 0, 0, nil, fmt.Errorf("bad magic 0x%x", m)
	}
	opcode = binary.BigEndian.Uint16(buf[4:6])
	channelID = binary.BigEndian.Uint32(buf[6:10])
	payload = buf[headerLen:]
	return opcode, channelID, payload, nil
}
