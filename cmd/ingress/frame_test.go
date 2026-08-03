package main

import (
	"bufio"
	"bytes"
	"errors"
	"testing"

	"github.com/tachyne/tachyne-common/protocol"
)

func TestReadCappedPacketRoundtrip(t *testing.T) {
	var buf bytes.Buffer
	if err := protocol.WritePacket(&buf, 0x00, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	p, err := readCappedPacket(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ID != 0x00 || !bytes.Equal(p.Data, []byte{1, 2, 3}) {
		t.Fatalf("got id=%d data=%v, want id=0 data=[1 2 3]", p.ID, p.Data)
	}
}

func TestReadCappedPacketTooBig(t *testing.T) {
	// A frame length one over the cap, with NO body: because we reject on the
	// length prefix before allocating or reading, the absent body is never
	// touched — proving the huge allocation is avoided.
	var buf bytes.Buffer
	buf.Write(protocol.AppendVarInt(nil, maxPreSpliceFrame+1))
	_, err := readCappedPacket(bufio.NewReader(&buf))
	if !errors.Is(err, errFrameTooBig) {
		t.Fatalf("got %v, want errFrameTooBig", err)
	}
}

func TestReadCappedPacketAtCap(t *testing.T) {
	// A frame exactly at the cap must be accepted.
	body := make([]byte, maxPreSpliceFrame-1) // +1 for the packet-ID VarInt = cap
	var buf bytes.Buffer
	if err := protocol.WritePacket(&buf, 0x00, body); err != nil {
		t.Fatal(err)
	}
	if _, err := readCappedPacket(bufio.NewReader(&buf)); err != nil {
		t.Fatalf("frame at the cap should be accepted, got %v", err)
	}
}
