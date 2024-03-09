package mux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	MaxStreamNum       = 1<<31 - 1
	InitialStreamQuota = 100

	defaultKeepaliveTimeout = time.Second * 10
)

var (
	ErrSessionClosed     = errors.New("session closed")
	ErrSessionEoL        = errors.New("session end of life")
	ErrStreamQuotaLimit  = errors.New("stream quota limit")
	ErrTooManyStreams    = errors.New("too many streams")
	ErrPayloadOverflowed = errors.New("payload size is to large")
	ErrKeepaliveTimedOut = errors.New("keepalive ping timed out")
	ErrIdleTimedOut      = errors.New("idle timed out")
	ErrAcceptNotAllowed  = errors.New("accept is not allowed")

	errInvalidSid             = errors.New("invalid stream id")
	errDuplicatedSid          = errors.New("duplicated stream id")
	errStreamQuotaOverFlowed  = errors.New("stream quota overflowed")
	errNegativeStreamQuotaInc = errors.New("negative stream quota inc")
	errStreamQuotaViolation   = errors.New("stream quota violation")
)

type Opts struct {
	// AllowAccept indicates this Session can accept streams
	// from peer. If AllowAccept is false and peer sends a SYN
	// frame, local will send a FIN frame.
	// On serve this typically should be true and false on client.
	AllowAccept bool

	// StreamReceiveWindow sets the default size of receive window when
	// a stream was opened/accepted.
	// Minimum rx window size is 64k and maximum is (1<<32 - 1).
	// If StreamReceiveWindow is invalid, the closest limit will
	// be used. Which means a zero value is 64k.
	StreamReceiveWindow int32

	// Write buffer size. Default is about 64k.
	WriteBufferSize int

	// Read buffer size. Default is 64k.
	ReadBufferSize int

	// KeepaliveInterval indicates how long will this Session sends a
	// ping request to the peer if no data was received. Zero value means no ping will be sent.
	KeepaliveInterval time.Duration

	// KeepaliveTimeout indicates how long will this Session be closed with
	// ErrPingTimeout if no further data (any data, not just a pong) was
	// received after a keepalive ping was sent.
	// Default is 10s.
	KeepaliveTimeout time.Duration

	// IdleTimeout indicates how long will this Session be closed with
	// ErrIdleTimeout if no stream is alive.
	// Zero value means no idle timeout.
	IdleTimeout time.Duration

	// WriteTimeout is the timeout for write op. If a write op started and after
	// WriteTimeout no data was been written, the connection will be closed.
	// This requires the connection implementing [SetWriteDeadline(t time.Time) error] method.
	// See [net.Conn] for more details.
	WriteTimeout time.Duration

	// The number of concurrent streams that peer can open at a time.
	// Minimum is initialStreamQuota.
	MaxConcurrentStreams int32

	// OnClose will be called once when the session is closed.
	OnClose func(session *Session, err error)

	// Don't send pong back, for ping test.
	testNoPong bool
	// Send syn even if no local quota, for quota test.
	noLocalStreamQuotaLimit bool
	// Don't update peer stream quota, for quota test.
	noStreamQuotaUpdate bool
}

type Session struct {
	c    io.ReadWriteCloser
	opts Opts

	ctx    context.Context
	cancel context.CancelCauseFunc

	frameReader *frameReader // not concurrent safe. Used only in readLoop()
	frameWriter *frameWriter // concurrent safe.

	acceptedChan chan *Stream

	// m protects the following fields
	m                sync.RWMutex
	closed           bool
	openedSid        int32
	reserved         int32
	localStreamQuota int32 // number of streams which we can open
	peerStreamQuota  int32 // number of streams which peer can open

	streams   map[int32]*Stream // negative id is opened by peer
	idleTimer *time.Timer       // nil if no idle timeout was set. Reset only if !closed.

	pingCtl   *pingCtl
	readEvent chan struct{} // for keepalive check
}

type SessionStatus struct {
	Closed           bool
	OpenedSid        int32
	Reserved         int32
	LocalStreamQuota int32
	PeerStreamQuota  int32
	ActiveStreams    int
}

func NewSession(c io.ReadWriteCloser, opts Opts) *Session {
	ctx, cancel := context.WithCancelCause(context.Background())
	s := &Session{
		c:           c,
		opts:        opts,
		ctx:         ctx,
		cancel:      cancel,
		frameReader: newFrameReader(c, opts.ReadBufferSize),

		acceptedChan: make(chan *Stream, 1),

		streams:          make(map[int32]*Stream),
		localStreamQuota: InitialStreamQuota,
		peerStreamQuota:  InitialStreamQuota,
		pingCtl:          newPingCtl(),
		readEvent:        make(chan struct{}),
	}
	s.frameWriter = newFrameWriter(c, opts.WriteBufferSize, s.opts.WriteTimeout)

	if d := opts.IdleTimeout; d > 0 {
		s.idleTimer = time.AfterFunc(opts.IdleTimeout, s.closeIfIdle)
	}

	go s.readLoop()
	return s
}

// SubConn returns the io.ReadWriteCloser that created this Session.
// This is for accessing info only. DO NOT r/w/c this sub connection.
func (s *Session) SubConn() io.ReadWriteCloser {
	return s.c
}

// Returned [context.Context] will be canceled with the cause error when [Session]
// is dead.
func (s *Session) Context() context.Context {
	return s.ctx
}

// OpenStream opens a stream.
// Returns:
// ErrClosedSession if Session was closed.
// ErrStreamIdOverFlowed if Session has opened too many streams (see MaxStreamNum).
// Any error that inner connection returns while sending syn frame.
func (s *Session) OpenStream() (*Stream, error) {
	s.m.Lock()
	if s.closed {
		s.m.Unlock()
		return nil, ErrSessionClosed
	}

	if s.openedSid == MaxStreamNum {
		s.m.Unlock()
		return nil, ErrSessionEoL
	}

	if s.localStreamQuota <= 0 && !s.opts.noLocalStreamQuotaLimit {
		s.m.Unlock()
		return nil, ErrStreamQuotaLimit
	}
	s.openedSid++
	s.localStreamQuota--
	if s.reserved > 0 {
		s.reserved--
	}
	sid := s.openedSid
	stream := newStream(s, sid)
	s.streams[sid] = stream
	s.m.Unlock()

	s.frameWriter.WriteSyn(stream.ID())
	stream.SetRxWindowSize(validWindowSize(s.opts.StreamReceiveWindow))

	if err := s.frameWriter.Err(); err != nil {
		return nil, err
	}
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
	case <-s.ctx.Done():
		return nil, context.Cause(s.ctx)
	}
}

// ReserveStreamN tries to reserve N streams for future use.
// If session reaches the stream id limit, the returned [reserved]
// will be smaller than n.
func (s *Session) ReserveStreamN(n int32) (reserved int32) {
	if n < 0 {
		panic("negative reserve")
	}

	s.m.Lock()
	defer s.m.Unlock()

	eofLimit := MaxStreamNum - s.openedSid - s.reserved
	reserved = min(eofLimit, n)
	s.reserved += reserved
	return reserved
}

func (s *Session) Status() SessionStatus {
	s.m.RLock()
	defer s.m.RUnlock()
	return SessionStatus{
		Closed:           s.closed,
		OpenedSid:        s.openedSid,
		Reserved:         s.reserved,
		LocalStreamQuota: s.localStreamQuota,
		PeerStreamQuota:  s.peerStreamQuota,
		ActiveStreams:    len(s.streams),
	}
}

// Close closes Session and all its Streams. It always returns nil.
func (s *Session) Close() error {
	s.close(nil, false)
	return nil
}

func (s *Session) CloseWithErr(err error) {
	s.close(err, false)
}

func (s *Session) close(err error, byIdleTimer bool) {
	if err == nil {
		err = ErrSessionClosed
	}

	s.m.Lock()
	if s.closed ||
		(byIdleTimer && len(s.streams) > 0 || s.reserved > 0) { // close if idle
		s.m.Unlock()
		return
	}
	s.closed = true

	if !byIdleTimer {
		// Don't access idleTimer in this func if it was called by it.
		if s.idleTimer != nil {
			s.idleTimer.Stop()
		}
	}

	s.cancel(err)
	for _, stream := range s.streams {
		stream.closeWithErr(err, false)
	}
	s.m.Unlock()

	// Call those funcs outside of lock because they may block.
	s.frameWriter.WriteGoAway(0)
	s.c.Close()
	if fn := s.opts.OnClose; fn != nil {
		fn(s, err)
	}
}

// called by s.idleTimer.
func (s *Session) closeIfIdle() {
	s.close(ErrIdleTimedOut, true)
}

func (s *Session) keepaliveTimeout() time.Duration {
	if d := s.opts.KeepaliveTimeout; d > 0 {
		return d
	}
	return defaultKeepaliveTimeout
}

// keepalive sends a keepalive ping.
// If ping timed out or failed, It closes the connection.
func (s *Session) keepalive() {
	ctx, cancel := context.WithTimeoutCause(s.ctx, s.keepaliveTimeout(), ErrKeepaliveTimedOut)
	defer cancel()
	err := s.keepalivePing(ctx)
	if err != nil {
		s.CloseWithErr(err)
	}
}

func (s *Session) sendReadEvent() {
	select {
	case s.readEvent <- struct{}{}:
	default:
	}
}

func (s *Session) readLoop() {
	keepaliveInterval := s.opts.KeepaliveInterval
	var keepaliveTimer *time.Timer
	if keepaliveInterval != 0 {
		keepaliveTimer = time.AfterFunc(keepaliveInterval, s.keepalive)
		defer keepaliveTimer.Stop()
	}

	for {
		typ, err := s.frameReader.ReadTyp()
		if err != nil {
			s.CloseWithErr(fmt.Errorf("failed to read frame type header: %w", err))
			return
		}

		switch typ {
		case frameTypeSyn:
			if err := s.handleSynFrame(); err != nil {
				s.CloseWithErr(fmt.Errorf("failed to handle SYN frame: %w", err))
				return
			}
		case frameTypeData:
			if err := s.handleDataFrame(); err != nil {
				s.CloseWithErr(fmt.Errorf("failed to handle DATA frame: %w", err))
				return
			}
		case frameTypeFin:
			if err := s.handleFinFrame(); err != nil {
				s.CloseWithErr(fmt.Errorf("failed to handle FIN frame: %w", err))
				return
			}
		case frameTypePing:
			if err := s.handlePingFrame(); err != nil {
				s.CloseWithErr(fmt.Errorf("failed to handle PING frame: %w", err))
				return
			}
		case frameTypePong:
			if err := s.handlePongFrame(); err != nil {
				s.CloseWithErr(fmt.Errorf("failed to handle PONG frame: %w", err))
				return
			}
		case frameTypeWindowsUpdate:
			if err := s.handleWindowUpdateFrame(); err != nil {
				s.CloseWithErr(fmt.Errorf("failed to handle window update frame: %w", err))
				return
			}
		case frameTypeGoAway:
			if err := s.handleGoAwayFrame(); err != nil {
				s.CloseWithErr(fmt.Errorf("failed to handle go away frame: %w", err))
				return
			}
		case frameTypeStreamQuotaUpdate:
			if err := s.handleStreamQuotaUpdateFrame(); err != nil {
				s.CloseWithErr(fmt.Errorf("failed to handle stream quota frame: %w", err))
				return
			}
		default:
			s.CloseWithErr(fmt.Errorf("invalid frame type %d", typ))
			return
		}

		if keepaliveTimer != nil {
			keepaliveTimer.Reset(keepaliveInterval)
		}
		s.sendReadEvent()
	}
}

func (s *Session) handleSynFrame() error {
	sid, err := s.frameReader.ReadSYN()
	if err != nil {
		return err
	}
	sid = -sid

	if sid >= 0 { // Peer cannot open positive sid.
		return errInvalidSid
	}

	if !s.opts.AllowAccept {
		s.frameWriter.WriteFin(sid)
		return nil
	}

	s.m.Lock()
	if s.peerStreamQuota <= 0 {
		s.m.Unlock()
		return errStreamQuotaViolation
	}
	s.peerStreamQuota--

	_, dup := s.streams[sid]
	if dup {
		s.m.Unlock()
		return errDuplicatedSid
	}
	stream := newStream(s, sid)
	s.streams[sid] = stream
	if s.idleTimer != nil && len(s.streams) == 1 {
		s.idleTimer.Stop()
	}
	incPeerStreamQuota := s.incPeerStreamQuotaLocked()
	s.m.Unlock()

	stream.SetRxWindowSize(validWindowSize(s.opts.StreamReceiveWindow))
	if incPeerStreamQuota > 0 {
		s.frameWriter.WriteStreamQuotaUpdate(incPeerStreamQuota)
	}

	select {
	case s.acceptedChan <- stream:
	case <-s.ctx.Done():
		return context.Cause(s.ctx)
	}
	return s.frameWriter.Err()
}

func (s *Session) incPeerStreamQuotaLocked() int32 {
	if s.opts.noStreamQuotaUpdate {
		return 0
	}

	maxConcurrentStreams := s.opts.MaxConcurrentStreams
	if maxConcurrentStreams < InitialStreamQuota {
		maxConcurrentStreams = InitialStreamQuota
	}
	if delta := maxConcurrentStreams - s.peerStreamQuota; delta > (maxConcurrentStreams >> 2) {
		s.peerStreamQuota += delta
		return delta
	}
	return 0
}

func (s *Session) handleDataFrame() error {
	sid, l, err := s.frameReader.ReadDataHdr()
	if err != nil {
		return err
	}
	if l == 0 {
		return nil
	}

	sid = -sid
	s.m.RLock()
	stream := s.streams[sid]
	s.m.RUnlock()

	if stream == nil {
		// No such stream, maybe we closed it first. Just ignore it and discard
		// its payload.
		_, err := s.frameReader.DiscardPayload(int(l))
		return err
	}

	var n int
	for n < int(l) { // Read all frame payload
		_, nr, err := stream.rb.ReadChunk(s.frameReader.r, int(l)-n)
		n += nr
		if err != nil {
			return err
		}
	}
	return nil
}

// Removes sid from Session.
func (s *Session) removeStream(sid int32) *Stream {
	s.m.Lock()
	stream := s.streams[sid]
	if stream != nil {
		delete(s.streams, sid)
	}
	if s.idleTimer != nil && len(s.streams) == 0 && s.reserved == 0 && !s.closed {
		s.idleTimer.Reset(s.opts.IdleTimeout)
	}
	if s.openedSid == MaxStreamNum && len(s.streams) == 0 { // eol
		defer s.CloseWithErr(ErrSessionEoL) // defer out of the lock
	}
	s.m.Unlock()
	return stream
}

func (s *Session) handleFinFrame() error {
	sid, err := s.frameReader.ReadFin()
	if err != nil {
		return err
	}
	sid = -sid

	stream := s.removeStream(sid)
	if stream != nil {
		stream.closeWithErr(io.EOF, false)
	}
	return nil
}

func (s *Session) handleWindowUpdateFrame() error {
	sid, inc, err := s.frameReader.ReadWindowUpdate()
	if err != nil {
		return err
	}
	sid = -sid

	s.m.RLock()
	stream := s.streams[sid]
	s.m.RUnlock()
	if stream == nil { // No such stream. Ignore it.
		return nil
	}
	if err := stream.tc.IncWindow(inc); err != nil {
		return err
	}
	return nil
}

func (s *Session) handlePingFrame() error {
	id, err := s.frameReader.ReadPing()
	if err != nil {
		return err
	}

	if s.opts.testNoPong {
		return nil
	}
	_, err = s.frameWriter.WritePong(id)
	return err
}

func (s *Session) handlePongFrame() error {
	id, err := s.frameReader.ReadPing()
	if err != nil {
		return err
	}
	s.pingCtl.Notify(id)
	return nil
}

func (s *Session) handleGoAwayFrame() error {
	_, err := s.frameReader.ReadGoAway() // TODO: Impl error code handling.
	if err != nil {
		return err
	}
	// Assume that peer go away as EOF for all streams.
	s.CloseWithErr(io.EOF)
	return nil
}

func (s *Session) handleStreamQuotaUpdateFrame() error {
	inc, err := s.frameReader.ReadStreamQuotaUpdate()
	if err != nil {
		return err
	}

	if inc < 0 {
		return errNegativeStreamQuotaInc
	}

	s.m.Lock()
	if n, c := add32(s.localStreamQuota, inc); c > 0 {
		s.m.Unlock()
		return errStreamQuotaOverFlowed
	} else {
		s.localStreamQuota = n
	}
	s.m.Unlock()
	return nil
}

func (s *Session) Ping(ctx context.Context) error {
	id, c := s.pingCtl.Reg()
	defer s.pingCtl.Del(id)

	_, err := s.frameWriter.WritePing(id)
	if err != nil {
		return err
	}

	select {
	case <-c:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (s *Session) keepalivePing(ctx context.Context) error {
	id, _ := s.pingCtl.Reg()
	defer s.pingCtl.Del(id)

	_, err := s.frameWriter.WritePing(id)
	if err != nil {
		return err
	}

	select {
	case <-s.readEvent:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}
