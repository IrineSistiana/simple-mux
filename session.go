package mux

import (
	"bufio"
	"errors"
	"fmt"
	"github.com/urlesistiana/alloc-go"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const (
	MaxStreamNum = 1<<31 - 1
)

var (
	ErrClosedSession      = errors.New("closed session")
	ErrStreamIdOverFlowed = errors.New("stream id is overflowed")
	ErrInvalidSynFrame    = errors.New("invalid syn frame")
	ErrInvalidSID         = errors.New("invalid stream id")
	ErrPingTimeout        = errors.New("ping timed out")
	ErrIdleTimeout        = errors.New("idle timed out")
	ErrAcceptNotAllowed   = errors.New("accept is not allowed")
	ErrFlowWindowOverflow = errors.New("flow control window overflowed")
)

type Opts struct {
	// AllowAccept indicates this Session can accept streams
	// from peer. If AllowAccept is false and peer sends a SYN
	// frame, the Session will be closed with ErrInvalidSynFrame.
	AllowAccept bool

	// StreamReceiveWindow sets the default size of receive window when
	// a stream was opened/accepted.
	// Minimum rx window size is 64k and maximum is (1<<32 - 1).
	// If StreamReceiveWindow is invalid, the closest limit will
	// be used. Which means a zero value is 64k.
	StreamReceiveWindow uint32

	// PingInterval indicates how long will this Session sends a
	// ping request to the peer. Zero value means no ping will be sent.
	PingInterval time.Duration

	// PingTimeout indicates how long will this Session be closed with
	// ErrPingTimeout if no further data (any data, not just a pong) was
	// received after a ping was sent.
	// Default is 10s.
	// If PingTimeout > PingInterval, PingInterval will be used.
	PingTimeout time.Duration

	// IdleTimeout indicates how long will this Session be closed with
	// ErrIdleTimeout if no data (excluding ping and pong) was transmitted.
	// Zero value means no idle timeout.
	IdleTimeout time.Duration
}

type Session struct {
	c    io.ReadWriteCloser
	opts Opts

	acceptedChan chan *Stream

	// read loop
	pingReadMark atomic.Bool // for fast ping check

	// write loop
	// Once the writeFrameOp was sent to the writeOpChan,
	// c.Write will be called and the result will be
	// sent back by writeFrameOp.rc.
	writeOpChan chan writeFrameOp

	idleTimer *idleTimer // May be nil if idle timeout was not set.

	closeNotify chan struct{}
	closeErr    error // valid after closeNotify was closed.

	// m protects the following fields
	m          sync.RWMutex
	closed     atomic.Bool // atomic for fast checks without acquiring m.
	openedSid  int32
	reservedId int32
	streams    map[int32]*Stream // negative id is opened by peer
}

type writeFrameOp struct {
	b []byte
	// rc is the chan to receive write result.
	// If set, it must not block (should have at least one buf)
	rc chan writeRes
	// If keepIdle, session won't reset its idle timer.
	// (e.g. this is a PING frame)
	keepIdle bool
	// Indicate b is an alloc buffer and should be released.
	releaseB bool
}
type writeRes struct {
	n   int
	err error
}

func NewSession(c io.ReadWriteCloser, opts Opts) *Session {
	s := &Session{
		c:            c,
		opts:         opts,
		acceptedChan: make(chan *Stream, 1),
		writeOpChan:  make(chan writeFrameOp),
		closeNotify:  make(chan struct{}),
		streams:      make(map[int32]*Stream),
	}

	if t := opts.IdleTimeout; t > 0 {
		s.idleTimer = newIdleTimer(t, func() {
			s.closeWithErr(ErrIdleTimeout)
		})
	}

	go s.readLoop()
	go s.writeLoop()
	if s.opts.PingInterval > 0 {
		go s.pingLoop()
	}
	return s
}

// SubConn returns the io.ReadWriteCloser that created this Session.
// This is for accessing info only. DO NOT r/w/c this sub connection.
func (s *Session) SubConn() io.ReadWriteCloser {
	return s.c
}

// OpenStream opens a stream.
// Returns:
// ErrClosedSession if Session was closed.
// ErrStreamIdOverFlowed if Session has opened too many streams (see MaxStreamNum).
// Any error that inner connection returns while sending syn frame.
func (s *Session) OpenStream() (*Stream, error) {
	// allocate sid
	s.m.Lock()
	if s.Closed() {
		s.m.Unlock()
		return nil, ErrClosedSession
	}
	sid, ok := s.openGetNextSidLocked()
	if !ok {
		s.m.Unlock()
		return nil, ErrStreamIdOverFlowed
	}
	stream := newStream(s, sid)
	s.streams[sid] = stream
	s.m.Unlock()

	// send SYN and the first window update (if needed)
	if err := s.sendFrameBuf(packSynFrame(sid), false); err != nil {
		s.m.Lock()
		delete(s.streams, sid)
		s.m.Unlock()
		stream.closeWithErr(err, true)
		return nil, err
	}
	stream.SetRxWindowSize(validWindowSize(s.opts.StreamReceiveWindow))

	return stream, nil
}

// Accept accepts a Stream from peer.
// Session must be created with Opts.AllowAccept. Otherwise,
// Accept returns ErrAcceptNotAllowed.
// A Stream must be Accept-ed ASAP. Otherwise, all read operations of
// this Session (all its streams) will be blocked.
func (s *Session) Accept() (*Stream, error) {
	if !s.opts.AllowAccept {
		return nil, ErrAcceptNotAllowed
	}

	select {
	case sm := <-s.acceptedChan:
		return sm, nil
	case <-s.closeNotify:
		return nil, s.closeErr
	}
}

// ReserveStream reserves a stream id for the next OpenStream call.
// It returns false if stream id was overflowed. (> MaxStreamNum)
func (s *Session) ReserveStream() bool {
	s.m.Lock()
	defer s.m.Unlock()
	if s.openedSid+s.reservedId == MaxStreamNum {
		return false
	}
	s.reservedId++
	return true
}

// OngoingStreams reports how many streams are currently in this Session.
func (s *Session) OngoingStreams() int {
	s.m.RLock()
	defer s.m.RUnlock()
	return len(s.streams)
}

// Close closes Session and all its Stream-s.
func (s *Session) Close() error {
	s.closeWithErr(ErrClosedSession)
	return nil
}

// Closed reports whether this Session was closed.
// This is a faster way than checking CloseErr.
func (s *Session) Closed() bool {
	return s.closed.Load()
}

// CloseErr returns the error that closes the Session.
// If Session wasn't closed, it returns nil.
func (s *Session) CloseErr() error {
	select {
	case <-s.closeNotify:
		return s.closeErr
	default:
		return nil
	}
}

func (s *Session) closeWithErr(err error) {
	if err == nil {
		err = ErrClosedSession
	}

	s.m.Lock()
	defer s.m.Unlock()
	if s.closed.Load() {
		return
	}
	s.closed.Store(true)
	s.closeErr = err
	close(s.closeNotify)
	_ = s.c.Close()

	if s.idleTimer != nil {
		s.idleTimer.stop()
	}

	for _, stream := range s.streams {
		stream.closeWithErr(err, true)
	}
}

type readFunc func([]byte) (int, error)

func (r readFunc) Read(p []byte) (n int, err error) {
	return r(p)
}

func (s *Session) readLoop() {
	br := bufio.NewReaderSize(s.c, 128)
	var r readFunc = func(b []byte) (int, error) {
		n, err := br.Read(b)
		if n > 0 {
			s.pingReadMark.Store(true)
		}
		return n, err
	}

	hb := make([]byte, 1)
	for {
		if _, err := io.ReadFull(r, hb); err != nil {
			s.closeWithErr(fmt.Errorf("failed to read header: %w", err))
			return
		}
		typ := frameType(hb[0])

		keepIdle := false
		switch typ {
		case frameTypeSYN:
			if err := s.handleSYN(r); err != nil {
				s.closeWithErr(fmt.Errorf("failed to handle syn cmd: %w", err))
				return
			}
		case frameTypeFIN:
			if err := s.handleFIN(r); err != nil {
				s.closeWithErr(fmt.Errorf("failed to handle fin sid: %w", err))
				return
			}

		case frameTypeData:
			if err := s.handleDataFrame(r); err != nil {
				s.closeWithErr(fmt.Errorf("failed to handle data frame: %w", err))
				return
			}
		case frameTypePing:
			keepIdle = true
			if err := s.sendFrameBuf(packPongFrame(), true); err != nil {
				s.closeWithErr(fmt.Errorf("failed to handle PING cmd: %w", err))
				return
			}
		case frameTypePong:
			keepIdle = true
		case frameTypeWindowsUpdate:
			if err := s.handleWindowUpdateFrame(r); err != nil {
				s.closeWithErr(fmt.Errorf("failed to handle window update cmd: %w", err))
				return
			}
		default:
			s.closeWithErr(fmt.Errorf("invalid cmd %d", typ))
			return
		}

		if !keepIdle && s.idleTimer != nil {
			s.idleTimer.reset()
		}
	}
}

func (s *Session) handleSYN(r io.Reader) error {
	if !s.opts.AllowAccept {
		return ErrInvalidSynFrame
	}

	sid, err := readSid(r)
	if err != nil {
		return err
	}

	sid = -sid
	if sid >= 0 {
		return ErrInvalidSID
	}

	sm := newStream(s, sid)
	s.m.Lock()
	_, dup := s.streams[sid]
	if dup { // duplicated sid
		s.m.Unlock()
		sm.closeWithErr(ErrInvalidSID, true)
		return ErrInvalidSID
	}
	s.streams[sid] = sm
	s.m.Unlock()
	sm.SetRxWindowSize(validWindowSize(s.opts.StreamReceiveWindow))

	select {
	case s.acceptedChan <- sm:
	case <-s.closeNotify:
		return s.closeErr
	}

	return nil
}

func (s *Session) handleFIN(r io.Reader) error {
	sid, err := readSid(r)
	if err != nil {
		return err
	}
	sid = -sid

	s.m.Lock()
	sm := s.streams[sid]
	if sm == nil {
		// streams has been closed. Nothing to do.
		s.m.Unlock()
		return nil
	}
	delete(s.streams, sid)
	s.m.Unlock()
	sm.closeWithErr(io.EOF, true)
	return nil
}

func (s *Session) handleDataFrame(r io.Reader) error {
	sid, l, err := readDataHeader(r)
	if err != nil {
		return err
	}
	if l == 0 {
		return nil
	}
	sid = -sid

	b := alloc.Get(int(l))
	n := 0
	for n < int(l) {
		nr, err := r.Read(b[n:])
		if nr > 0 {
			if int(l) == nr { // full read
				return s.pushData(sid, allocBuffer{b: b})
			}
			// partial frame read
			// push what we have read to the stream asap for lower latency
			fragment := alloc.Get(nr)
			copy(fragment, b[n:])
			if err := s.pushData(sid, allocBuffer{b: fragment}); err != nil {
				alloc.Release(b)
				return err
			}
		}
		if err != nil {
			alloc.Release(b)
			return err
		}
		n += nr
	}
	alloc.Release(b)
	return nil
}

// pushData will take b's ownership.
func (s *Session) pushData(sid int32, b allocBuffer) error {
	s.m.RLock()
	sm := s.streams[sid]
	s.m.RUnlock()
	if sm == nil { // stream has been closed.
		return nil
	}

	overflowed, closed := sm.rb.pushBuffer(b)
	if overflowed || closed {
		releaseBuffer(b)
		if overflowed {
			return ErrFlowWindowOverflow // receive window overflowed is a protocol error
		}
	}
	return nil
}

func (s *Session) handleWindowUpdateFrame(r io.Reader) error {
	sid, i, err := readWindowUpdate(r)
	if err != nil {
		return err
	}
	if i > MaxWindow {
		return ErrFlowWindowOverflow
	}
	sid = -sid

	s.m.RLock()
	sm := s.streams[sid]
	s.m.RUnlock()
	if sm == nil {
		return nil
	}
	if ok := sm.flowControl.inc(i); !ok {
		return fmt.Errorf("cannot increase window size by %d, overflowed", i)
	}
	return nil
}

func (s *Session) writeLoop() {
	for {
		select {
		case op := <-s.writeOpChan:
			n, err := s.c.Write(op.b)
			if rc := op.rc; rc != nil {
				select {
				case rc <- writeRes{n: n, err: err}:
				default:
					panic("rc is blocked")
				}
			}
			if op.releaseB {
				alloc.Release(op.b)
			}
			if err != nil {
				s.closeWithErr(fmt.Errorf("write loop exited: %w", err))
				return
			}
			if !op.keepIdle && s.idleTimer != nil {
				s.idleTimer.reset()
			}
		case <-s.closeNotify:
			return
		}
	}
}

func (s *Session) pingLoop() {
	interval := s.opts.PingInterval
	timeout := s.opts.PingTimeout
	if timeout > interval {
		timeout = interval
	}

	pingTicker := time.NewTicker(interval)
	defer pingTicker.Stop()
	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()

	resetTimeoutTimer := func() {
		if !timeoutTimer.Stop() {
			select {
			case <-timeoutTimer.C:
			default:
			}
		}
		timeoutTimer.Reset(timeout)
	}

	for {
		select {
		case <-pingTicker.C:
			s.pingReadMark.Store(false)
			if err := s.sendFrameBuf(packPingFrame(), true); err != nil {
				return
			}
			resetTimeoutTimer()
			select {
			case <-timeoutTimer.C:
				if !s.pingReadMark.Load() { // no data was received after ping being sent.
					s.closeWithErr(ErrPingTimeout)
					return
				}
			case <-s.closeNotify:
				return
			}
		case <-s.closeNotify:
			return
		}
	}
}

// sendFrameBuf takes control of the b. If keepIdle, Session idle timer won't
// be reset. (e.g. ping)
func (s *Session) sendFrameBuf(b allocBuffer, keepIdle bool) error {
	rc := getWriteResChan()
	defer releaseWriteResChan(rc)

	select {
	case s.writeOpChan <- writeFrameOp{b: b.b, rc: rc, keepIdle: keepIdle, releaseB: true}:
		res := <-rc
		return res.err
	case <-s.closeNotify:
		return s.closeErr
	}
}

// streamCloseNotify must only be called by a stream that belongs to s when
// the stream is closing not by the session (e.g. user)
func (s *Session) streamCloseNotify(sid int32) {
	var sendFin2Peer bool
	s.m.Lock()
	// If session received FIN from peer first, this sid will no longer in the session,
	// We don't have to send FIN back.
	if _, ok := s.streams[sid]; ok {
		delete(s.streams, sid)
		sendFin2Peer = true
	}
	s.m.Unlock()
	if sendFin2Peer {
		_ = s.sendFrameBuf(packFinFrame(sid), false)
	}
}

func (s *Session) openGetNextSidLocked() (int32, bool) {
	if s.openedSid == MaxStreamNum {
		return 0, false // overflowed
	}
	s.openedSid++
	if s.reservedId > 0 {
		s.reservedId--
	}
	return s.openedSid, true
}
