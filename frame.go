package mux

import (
	"encoding/binary"
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

func packPayload(t frameType, sid int32, payload []byte) *[]byte {
	bl := len(payload)
	if bl > maxPayloadLength {
		panic("payload overflowed")
	}
	bp := bytesPool.Get(1 + 4 + 2 + bl)
	b := *bp
	b[0] = byte(t)
	binary.BigEndian.PutUint32(b[1:5], uint32(sid))
	binary.BigEndian.PutUint16(b[5:7], uint16(bl))
	copy(b[7:], payload)
	return bp
}

func packDataFrame(sid int32, b []byte) *[]byte {
	return packPayload(frameTypeData, sid, b)
}

func packSynFinFrame(t frameType, sid int32) *[]byte {
	bp := bytesPool.Get(1 + 4)
	b := *bp
	b[0] = byte(t)
	binary.BigEndian.PutUint32(b[1:5], uint32(sid))
	return bp
}

func packSynFrame(sid int32) *[]byte {
	return packSynFinFrame(frameTypeSYN, sid)
}

func packFinFrame(sid int32) *[]byte {
	return packSynFinFrame(frameTypeFIN, sid)
}

func packPingPongFrame(t frameType) *[]byte {
	b := bytesPool.Get(1)
	(*b)[0] = byte(t)
	return b
}

func packPingFrame() *[]byte {
	return packPingPongFrame(frameTypePing)
}

func packPongFrame() *[]byte {
	return packPingPongFrame(frameTypePong)
}

func packWindowUpdateFrame(sid int32, i uint32) *[]byte {
	bp := bytesPool.Get(1 + 4 + 4)
	b := *bp
	b[0] = byte(frameTypeWindowsUpdate)
	binary.BigEndian.PutUint32(b[1:5], uint32(sid))
	binary.BigEndian.PutUint32(b[5:9], i)
	return bp
}

func readSid(r io.Reader) (int32, error) {
	bp := bytesPool.Get(4)
	defer bytesPool.Release(bp)

	b := *bp
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, err
	}
	return int32(binary.BigEndian.Uint32(b)), nil
}

func readWindowUpdate(r io.Reader) (sid int32, i uint32, err error) {
	bp := bytesPool.Get(8)
	defer bytesPool.Release(bp)

	b := *bp
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, 0, err
	}
	return int32(binary.BigEndian.Uint32(b[:4])), binary.BigEndian.Uint32(b[4:]), nil
}

func readDataHeader(r io.Reader) (sid int32, l uint16, err error) {
	bp := bytesPool.Get(6)
	defer bytesPool.Release(bp)

	b := *bp
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, 0, err
	}
	return int32(binary.BigEndian.Uint32(b[:4])), binary.BigEndian.Uint16(b[4:]), nil
}
