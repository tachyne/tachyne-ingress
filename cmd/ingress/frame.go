package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"

	"github.com/tachyne/tachyne-common/protocol"
)

// maxPreSpliceFrame caps every packet ingress reads before it starts splicing:
// the Java handshake and the status/ping packets it answers locally for
// unsupported versions. A real handshake is a few hundred bytes (even Forge's
// FML marker in the address field stays tiny); the cap exists only to stop a
// client from declaring a huge VarInt frame length and forcing a matching
// allocation before we've read a single useful byte. Real gameplay payload
// never flows through here — once routed, io.Copy moves those bytes untouched.
const maxPreSpliceFrame = 32 << 10 // 32 KiB

var errFrameTooBig = errors.New("ingress: packet frame exceeds cap")

// readCappedPacket reads one uncompressed frame like protocol.ReadPacket, but
// rejects a declared length outside [0, maxPreSpliceFrame] before allocating
// it, so a hostile length prefix costs nothing.
func readCappedPacket(br *bufio.Reader) (*protocol.Packet, error) {
	length, err := protocol.ReadVarInt(br)
	if err != nil {
		return nil, err
	}
	if length < 0 || length > maxPreSpliceFrame {
		return nil, errFrameTooBig
	}
	frame := make([]byte, length)
	if _, err := io.ReadFull(br, frame); err != nil {
		return nil, err
	}
	fr := bytes.NewReader(frame)
	id, err := protocol.ReadVarInt(fr)
	if err != nil {
		return nil, err
	}
	data := make([]byte, fr.Len())
	if _, err := io.ReadFull(fr, data); err != nil {
		return nil, err
	}
	return &protocol.Packet{ID: id, Data: data}, nil
}
