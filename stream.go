package mux

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/IrineSistiana/simple-mux/internal/flowctl"
)

const (
	initialStreamWindow = 64*1024 - 1
)

var (
	ErrClosedStream = errors.New("closed stream")
)

type Stream struct {
	// constant
	id      int32
	session *Session

	ctx    context.Context
	cancel context.CancelCauseFunc

	// read
	rb *flowctl.RxBuffer

	// write
	tc *flowctl.TxCtrl

	closeOnce sync.Once
}

func newStream(sess *Session, sid int32) *Stream {
	s := &Stream{
		id:      sid,
		session: sess,
		rb:      flowctl.NewRxBuffer(initialStreamWindow),
		tc:      flowctl.NewTxCtrl(initialStreamWindow),
	}
	s.ctx, s.cancel = context.WithCancelCause(sess.Context())
	return s
}

// ID returns the stream's id. Negative id means the stream is opened
// by peer.
func (s *Stream) ID() int32 {
	return s.id
}

// Session returns the Session that this Stream is belonged to.
func (s *Stream) Session() *Session {
	return s.session
}

// If stream was closed, the context.Context will be canceled with the cause error.
func (s *Stream) Context() context.Context {
	return s.ctx
}

// Read implements io.Reader.
func (s *Stream) Read(p []byte) (n int, err error) {
	return s.read(p)
}

func (s *Stream) read(p []byte) (n int, err error) {
	stat, n, err := s.rb.Read(p)
	if err != nil {
		return n, err
	}
	inc := needIncPeerWindow(stat)
	if inc > 0 {
		s.rb.IncWindow(inc)
		s.sendWindowUpdate(inc)
	}
	return n, nil
}

// To avoid dense ack. We only inc peer window after every 1/8 of the maximum window size was consumed.
func needIncPeerWindow(stat flowctl.RxStatus) int32 {
	pendingInc := stat.WindowMax - stat.WindowRemained - stat.Size
	if pendingInc > (stat.WindowMax >> 3) {
		return pendingInc
	}
	return 0
}

func (s *Stream) WriteTo(w io.Writer) (int64, error) {
	var n int64
	for {
		stat, nw, err := s.rb.WriteChunk(w)
		n += int64(nw)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			return n, nil
		}

		inc := needIncPeerWindow(stat)
		if inc > 0 {
			s.rb.IncWindow(inc)
			s.sendWindowUpdate(inc)
		}
	}
}

func validWindowSize(n int32) int32 {
	if n < initialStreamWindow {
		n = initialStreamWindow
	}
	return n
}

// SetRxWindowSize sets the stream rx windows size.
// If n is invalid, the default/minimum limit will be used.
// It returns an error if it cannot send a window update frame.
func (s *Stream) SetRxWindowSize(n int32) error {
	stat := s.rb.SetMaxWindow(validWindowSize(n))
	inc := needIncPeerWindow(stat)
	if inc > 0 {
		s.rb.IncWindow(inc)
		_, err := s.sendWindowUpdate(inc)
		return err
	}
	return nil
}

// Write implements io.Writer.
func (s *Stream) Write(p []byte) (n int, err error) {
	return s.write(p)
}

// write b to s in one or more data frames.
func (s *Stream) write(b []byte) (n int, err error) {
	for n < len(b) {
		nw, err := s.writeDataFrame(b[n:])
		n += nw
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (s *Stream) sendWindowUpdate(inc int32) (int, error) {
	return s.session.frameWriter.WriteWindowUpdate(s.id, inc)
}

// write a data frame to peer. It may not consume all b if b is too large
// for a frame or current flow window.
func (s *Stream) writeDataFrame(b []byte) (n int, err error) {
	ready, err := s.tc.ConsumeWindow(int32(min(len(b), maxPayloadLength)))
	if err != nil { // tc/stream closed
		return n, err
	}
	payload := b[:ready]
	return s.session.frameWriter.WriteData(s.id, payload)
}

// Close implements io.Closer.
// Close interrupts Read and Write.
func (s *Stream) Close() error {
	s.closeWithErr(nil, true)
	return nil
}

func (s *Stream) CloseWithErr(err error) {
	s.closeWithErr(err, true)
}

func (s *Stream) closeWithErr(err error, notifySession bool) {
	s.closeOnce.Do(func() {
		if err == nil {
			err = ErrClosedStream
		}
		s.rb.CloseWithErr(err)
		s.tc.CloseWithErr(err)

		s.cancel(err)

		if notifySession {
			s.session.removeStream(s.id)
			s.session.frameWriter.WriteFin(s.id)
		}
	})
}
