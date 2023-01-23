package mux

import (
	"encoding/binary"
	"github.com/urlesistiana/alloc-go"
	"io"
	"math"
)

type frameType uint8

const (
	maxPayloadLength = math.MaxUint16

	frameTypeInvalid frameType = 0
	frameTypeSYN     frameType = 1
	frameTypeFIN     frameType = 2
	frameTypeData    frameType = 3
	frameTypePing    frameType = 4
	frameTypePong    frameType = 5

	frameTypeWindowsUpdate frameType = 10
)

func packPayload(t frameType, sid int32, payload []byte) allocBuffer {
	bl := len(payload)
	if bl > maxPayloadLength {
		panic("payload overflowed")
	}
	b := alloc.Get(1 + 4 + 2 + bl)
	b[0] = byte(t)
	binary.BigEndian.PutUint32(b[1:5], uint32(sid))
	binary.BigEndian.PutUint16(b[5:7], uint16(bl))
	copy(b[7:], payload)
	return allocBuffer{b: b}
}

func packDataFrame(sid int32, b []byte) allocBuffer {
	return packPayload(frameTypeData, sid, b)
}

func packSynFinFrame(t frameType, sid int32) allocBuffer {
	b := alloc.Get(1 + 4)
	b[0] = byte(t)
	binary.BigEndian.PutUint32(b[1:5], uint32(sid))
	return allocBuffer{b: b}
}

func packSynFrame(sid int32) allocBuffer {
	return packSynFinFrame(frameTypeSYN, sid)
}

func packSynFrameWithWindowUpdate(sid int32, windowUpdate uint32) allocBuffer {
	b := alloc.Get(1 + 4 + 1 + 4 + 4)
	b[0] = byte(frameTypeSYN)
	binary.BigEndian.PutUint32(b[1:5], uint32(sid))
	b[5] = byte(frameTypeWindowsUpdate)
	binary.BigEndian.PutUint32(b[6:10], uint32(sid))
	binary.BigEndian.PutUint32(b[10:14], windowUpdate)
	return allocBuffer{b: b}
}

func packFinFrame(sid int32) allocBuffer {
	return packSynFinFrame(frameTypeFIN, sid)
}

func packPingPongFrame(t frameType) allocBuffer {
	b := alloc.Get(1)
	b[0] = byte(t)
	return allocBuffer{b: b}
}

func packPingFrame() allocBuffer {
	return packPingPongFrame(frameTypePing)
}

func packPongFrame() allocBuffer {
	return packPingPongFrame(frameTypePong)
}

func packWindowUpdateFrame(sid int32, i uint32) allocBuffer {
	b := alloc.Get(1 + 4 + 4)
	b[0] = byte(frameTypeWindowsUpdate)
	binary.BigEndian.PutUint32(b[1:5], uint32(sid))
	binary.BigEndian.PutUint32(b[5:9], i)
	return allocBuffer{b: b}
}

func readSid(r io.Reader) (int32, error) {
	b := alloc.Get(4)
	defer alloc.Release(b)
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, err
	}
	return int32(binary.BigEndian.Uint32(b)), nil
}

func readWindowUpdate(r io.Reader) (sid int32, i uint32, err error) {
	b := alloc.Get(8)
	defer alloc.Release(b)
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, 0, err
	}
	return int32(binary.BigEndian.Uint32(b[:4])), binary.BigEndian.Uint32(b[4:]), nil
}

func readDataHeader(r io.Reader) (sid int32, l uint16, err error) {
	b := alloc.Get(6)
	defer alloc.Release(b)
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, 0, err
	}
	return int32(binary.BigEndian.Uint32(b[:4])), binary.BigEndian.Uint16(b[4:]), nil
}
