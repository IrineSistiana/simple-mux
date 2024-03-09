package mux

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"sync"
	"time"
)

type frameType uint8

const (
	maxPayloadLength = math.MaxUint16

	frameTypeInvalid frameType = 0

	frameTypeSyn           frameType = 1
	frameTypeData          frameType = 2
	frameTypeWindowsUpdate frameType = 3
	frameTypeFin           frameType = 4

	frameTypeGoAway                      = 10
	frameTypeStreamQuotaUpdate frameType = 11

	frameTypePing frameType = 20
	frameTypePong frameType = 21
)

type frameWriter struct {
	l   sync.Mutex
	w   *bufio.Writer
	b   [9]byte
	err error // first write err
}

// protected by frameWriter.l
type stickyErrWriter struct {
	w       io.WriteCloser
	setDdl  ddlSetter // infer from w, maybe nil
	timeout time.Duration
	err     *error // pointer to frameWriter.err
}

type ddlSetter interface {
	SetWriteDeadline(t time.Time) error
}

// protected by frameWriter.l, no concurrent write.
func (sw *stickyErrWriter) Write(p []byte) (n int, err error) {
	if *sw.err != nil {
		return 0, *sw.err
	}

	for {
		if sw.setDdl != nil {
			sw.setDdl.SetWriteDeadline(time.Now().Add(sw.timeout))
		}

		nw, err := sw.w.Write(p[n:])
		n += nw
		if n < len(p) && nw > 0 && errors.Is(err, os.ErrDeadlineExceeded) {
			continue
		}

		if sw.setDdl != nil {
			sw.setDdl.SetWriteDeadline(time.Time{})
		}

		if err != nil {
			*sw.err = err
			sw.w.Close()
		}
		return n, err
	}
}

func newFrameWriter(w io.WriteCloser, bufSize int, writeTimeout time.Duration) *frameWriter {
	if bufSize <= 0 {
		// The maximum frame size is the data frame. (7 bytes header with full payload)
		bufSize = maxPayloadLength + 7
	}

	fw := &frameWriter{}
	sew := &stickyErrWriter{
		w:       w,
		timeout: writeTimeout,
		err:     &fw.err,
	}
	if writeTimeout > 0 {
		if dw, ok := w.(ddlSetter); ok {
			sew.setDdl = dw
		}
	}
	fw.w = bufio.NewWriterSize(sew, bufSize)
	return fw
}

func (fw *frameWriter) WriteSyn(sid int32) (int, error) {
	return fw.writeUint32(uint32(sid), frameTypeSyn)
}

// Write a data frame with payload b. len(b) must <= maxPayloadLength.
func (fw *frameWriter) WriteData(sid int32, b []byte) (int, error) {
	if len(b) > maxPayloadLength {
		return 0, ErrPayloadOverflowed
	}

	fw.l.Lock()
	defer fw.l.Unlock()

	fw.b[0] = byte(frameTypeData)
	binary.BigEndian.PutUint32(fw.b[1:5], uint32(sid))
	binary.BigEndian.PutUint16(fw.b[5:7], uint16(len(b)))

	_, err := fw.w.Write(fw.b[:7])
	if err != nil {
		return 0, err
	}
	n, err := fw.w.Write(b)
	if err != nil {
		return n, err
	}
	return n, fw.w.Flush()
}

func (fw *frameWriter) WriteFin(sid int32) (int, error) {
	return fw.writeUint32(uint32(sid), frameTypeFin)
}

func (fw *frameWriter) WriteWindowUpdate(sid int32, inc int32) (int, error) {
	fw.l.Lock()
	defer fw.l.Unlock()

	fw.b[0] = byte(frameTypeWindowsUpdate)
	binary.BigEndian.PutUint32(fw.b[1:5], uint32(sid))
	binary.BigEndian.PutUint32(fw.b[5:9], uint32(inc))
	n, err := fw.w.Write(fw.b[:9])
	if err != nil {
		return n, err
	}
	return n, fw.w.Flush()
}

func (fw *frameWriter) WritePing(id uint32) (int, error) {
	return fw.writeUint32(id, frameTypePing)
}

func (fw *frameWriter) WritePong(id uint32) (int, error) {
	return fw.writeUint32(id, frameTypePong)
}

func (fw *frameWriter) WriteGoAway(errCode uint32) (int, error) {
	return fw.writeUint32(errCode, frameTypeGoAway)
}

func (fw *frameWriter) WriteStreamQuotaUpdate(inc int32) (int, error) {
	return fw.writeUint32(uint32(inc), frameTypeStreamQuotaUpdate)
}

func (fw *frameWriter) writeUint32(u uint32, typ frameType) (int, error) {
	fw.l.Lock()
	defer fw.l.Unlock()

	fw.b[0] = byte(typ)
	binary.BigEndian.PutUint32(fw.b[1:5], u)
	n, err := fw.w.Write(fw.b[:5])
	if err != nil {
		return n, err
	}
	return n, fw.w.Flush()
}

func (fw *frameWriter) Err() error {
	fw.l.Lock()
	defer fw.l.Unlock()
	return fw.err
}

type frameReader struct {
	r *bufio.Reader
	b [8]byte
}

func newFrameReader(r io.Reader, bufSize int) *frameReader {
	if bufSize <= 0 {
		bufSize = maxPayloadLength
	}
	return &frameReader{r: bufio.NewReaderSize(r, bufSize)}
}

func (fr *frameReader) ReadTyp() (ft frameType, err error) {
	if _, err := io.ReadFull(fr.r, fr.b[:1]); err != nil {
		return 0, err
	}
	return frameType(fr.b[0]), nil
}

func (fr *frameReader) ReadSYN() (sid int32, err error) {
	u, err := fr.readUint32()
	return int32(u), err
}

func (fr *frameReader) ReadDataHdr() (sid int32, l uint16, err error) {
	if _, err := io.ReadFull(fr.r, fr.b[:6]); err != nil {
		return 0, 0, err
	}
	return int32(binary.BigEndian.Uint32(fr.b[:4])), binary.BigEndian.Uint16(fr.b[4:]), nil
}

func (fr *frameReader) ReaderForDataPayload() io.Reader {
	return fr.r
}

func (fr *frameReader) DiscardPayload(l int) (int, error) {
	return fr.r.Discard(l)
}

func (fr *frameReader) ReadFin() (sid int32, err error) {
	u, err := fr.readUint32()
	return int32(u), err
}

func (fr *frameReader) ReadWindowUpdate() (sid int32, inc int32, err error) {
	if _, err := io.ReadFull(fr.r, fr.b[:8]); err != nil {
		return 0, 0, err
	}
	return int32(binary.BigEndian.Uint32(fr.b[:4])), int32(binary.BigEndian.Uint32(fr.b[4:])), nil
}

func (fr *frameReader) ReadPing() (id uint32, err error) {
	return fr.readUint32()
}

func (fr *frameReader) ReadStreamQuotaUpdate() (inc int32, err error) {
	u, err := fr.readUint32()
	return int32(u), err
}

func (fr *frameReader) ReadGoAway() (errCode uint32, err error) {
	return fr.readUint32()
}

func (fr *frameReader) readUint32() (id uint32, err error) {
	if _, err := io.ReadFull(fr.r, fr.b[:4]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(fr.b[:4]), nil
}
