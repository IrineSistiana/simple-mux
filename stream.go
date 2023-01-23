package mux

import (
	"errors"
	"sync"
	"sync/atomic"
)

const (
	MinWindow = 64 * 1024
	MaxWindow = 1<<32 - 1
)

var (
	ErrClosedStream = errors.New("closed stream")
)

type Stream struct {
	// constant
	id   int32
	sess *Session

	// read
	rb       *rxBuffer
	rxWindow atomic.Uint32

	// write
	flowControl *outflowControl

	closeOnce   sync.Once
	closeNotify chan struct{}
	closeErr    error
}

func newStream(sess *Session, sid int32) *Stream {
	s := &Stream{
		id:          sid,
		sess:        sess,
		rb:          newRxBuffer(sess.defaultStreamRw),
		flowControl: newOutflowControl(),
		closeNotify: make(chan struct{}),
	}
	s.rxWindow.Store(sess.defaultStreamRw)
	return s
}

// ID returns the stream's id. Negative id means the stream is opened
// by peer.
func (s *Stream) ID() int32 {
	return s.id
}

// Session returns the Session that this Stream is belonged to.
func (s *Stream) Session() *Session {
	return s.sess
}

// Read implements io.Reader.
func (s *Stream) Read(p []byte) (n int, err error) {
	return s.read(p)
}

// ReadBufferSize returns the current buffer size that needs to be read.
// Useful to determine the buffer size for the next Read.
func (s *Stream) ReadBufferSize() int {
	return s.rb.len()
}

// SetRxWindowSize sets the stream rx windows size.
// If n is invalid, the closest limit will be used.
func (s *Stream) SetRxWindowSize(n uint32) {
	s.rxWindow.Store(validWindowSize(n))
}

func (s *Stream) read(p []byte) (n int, err error) {
	n, windowC, ok := s.rb.read(p, s.closeNotify)
	if !ok {
		return n, s.closeErr
	}

	window := s.rxWindow.Load()
	minUpdate := window / 8 // avoid dense ack

	// update peer window
	if ackPending := window - windowC; window > windowC && // avoid negative
		ackPending >= minUpdate {
		s.rb.windowInc(ackPending)
		s.updatePeerWindow(ackPending)
	}
	return n, err
}

func (s *Stream) updatePeerWindow(i uint32) {
	// ignore write error.
	// if session has a write error, it will close this stream anyway.
	_, _ = s.writeFramePayloadToSess(packWindowUpdateFrame(s.id, i))
}

// Write implements io.Writer.
func (s *Stream) Write(p []byte) (n int, err error) {
	return s.write(p)
}

func (s *Stream) write(p []byte) (n int, err error) {
	select {
	case <-s.closeNotify:
		return n, s.closeErr
	default:
	}

	for {
		frameLen := len(p) - n
		if frameLen > maxPayloadLength {
			frameLen = maxPayloadLength
		}
		window, ok := s.flowControl.consume(uint32(frameLen), s.closeNotify)
		if !ok { // stream closed
			return n, s.closeErr
		}
		frameLen = int(window)

		buf := packDataFrame(s.id, p[n:n+frameLen])
		headerLen := buf.len() - frameLen
		nw, err := s.writeFramePayloadToSess(buf)
		if err != nil {
			if nd := nw - headerLen; nd > 0 {
				n += nd
			}
			return n, err
		}
		n += frameLen
		if n == len(p) {
			break
		}
	}
	return n, nil
}

func (s *Stream) writeFramePayloadToSess(b allocBuffer) (int, error) {
	rc := getWriteResChan()
	select {
	case s.sess.writeOpChan <- writeOp{b: b.b, rc: rc, releaseB: true}:
		select {
		case res := <-rc:
			releaseWriteResChan(rc)
			return res.n, res.err
		case <-s.closeNotify:
			return 0, s.closeErr
		}
	case <-s.closeNotify:
		releaseWriteResChan(rc)
		return 0, s.closeErr
	}
}

// Close implements io.Closer.
// Close interrupts Read and Write.
func (s *Stream) Close() error {
	s.closeWithErr(ErrClosedStream, false)
	return nil
}

func (s *Stream) closeWithErr(err error, bySession bool) {
	s.closeOnce.Do(func() {
		if err == nil {
			err = ErrClosedStream
		}

		s.closeErr = err
		close(s.closeNotify)
		s.rb.close()

		if !bySession {
			s.sess.streamCloseNotify(s.id)
		}
	})
}

type outflowControl struct {
	m      sync.Mutex
	window uint32
	wakeUp chan struct{}
}

func newOutflowControl() *outflowControl {
	wc := &outflowControl{window: MinWindow, wakeUp: make(chan struct{}, 1)}
	return wc
}

func (wc *outflowControl) inc(i uint32) bool {
	wc.m.Lock()
	nw := wc.window + i
	if nw > MaxWindow { // overflowed
		wc.m.Unlock()
		return false
	}
	wc.window = nw
	wc.m.Unlock()
	wc.signal()
	return true
}

func (wc *outflowControl) signal() {
	select {
	case wc.wakeUp <- struct{}{}:
	default:
	}
}

func (wc *outflowControl) consume(s uint32, cancel <-chan struct{}) (uint32, bool) {
	wakenUp := false
consume:
	wc.m.Lock()
	if !wakenUp {
		select { // clear signal
		case <-wc.wakeUp:
		default:
		}
	}
	if wc.window >= s { // window is large enough for s.
		wc.window -= s
		hasMoreWindow := wc.window > 0
		wc.m.Unlock()
		if wakenUp && hasMoreWindow {
			// wake up another goroutine
			wc.signal()
		}
		return s, true
	}
	if wc.window != 0 && wc.window < s { // window is not large enough and is depleted by s
		consumed := wc.window
		wc.window = 0
		select {
		case <-wc.wakeUp:
		default:
		}
		wc.m.Unlock()
		return consumed, true
	}
	wc.m.Unlock()

	// window is 0. wait for inc()
	select {
	case <-wc.wakeUp:
		wakenUp = true
		goto consume
	case <-cancel:
		return 0, false
	}
}

type rxBuffer struct {
	m          sync.Mutex
	pushNotify chan struct{}
	window     uint32
	size       int
	head       *linkBuffer
	tail       *linkBuffer
	closed     bool
}

func newRxBuffer(window uint32) *rxBuffer {
	return &rxBuffer{
		window:     window,
		pushNotify: make(chan struct{}, 1),
	}
}

func (r *rxBuffer) read(p []byte, cancel <-chan struct{}) (n int, window uint32, ok bool) {
	wakenUp := false
read:
	r.m.Lock()
	if !wakenUp {
		select {
		case <-r.pushNotify:
		default:
		}
	}

	// read from buffers
	for {
		lb := r.head
		if lb == nil {
			break // the whole buffer is empty
		}
		if lb.read == len(lb.b) {
			// this link buffer is depleted, next
			next := lb.next
			releaseLinkBuffer(lb)
			if next == nil { // end of buffer
				r.head = nil
				r.tail = nil
				break
			}
			r.head = next
			continue
		}
		nr := copy(p[n:], lb.b[lb.read:])
		n += nr
		lb.read += nr
		r.size -= nr
		r.window -= uint32(nr)
		if n == len(p) { // p is full
			break
		}
	}
	window = r.window
	r.m.Unlock()

	if n > 0 { // return what have been read.
		return n, window, true
	}

	// empty buffer, wait
	select {
	case <-r.pushNotify:
		wakenUp = true
		goto read
	case <-cancel:
		return 0, 0, false
	}
}

func (r *rxBuffer) windowInc(i uint32) {
	r.m.Lock()
	defer r.m.Unlock()
	r.window += i
}

// pushBuffer takes control of ab if it returns false, false
func (r *rxBuffer) pushBuffer(ab allocBuffer) (overflowed bool, closed bool) {
	b := ab.b

	r.m.Lock()
	if r.closed {
		r.m.Unlock()
		return false, true
	}
	if bufSize := r.size + len(b); bufSize > int(r.window) {
		r.m.Unlock()
		return true, false
	} else {
		r.size = bufSize
	}

	lb := getLinkBuffer()
	lb.b = b
	if r.tail == nil {
		r.head = lb
		r.tail = lb
	} else {
		r.tail.next = lb
		r.tail = lb
	}

	select {
	case r.pushNotify <- struct{}{}:
	default:
	}
	r.m.Unlock()
	return false, false
}

func (r *rxBuffer) len() (n int) {
	r.m.Lock()
	defer r.m.Unlock()
	return r.size
}

// close closes rxBuffer and prevent further pushBuffer calls.
// Note: close does not interpret read. read should be canceled manually.
func (r *rxBuffer) close() {
	r.m.Lock()
	defer r.m.Unlock()
	r.closed = true
}

type linkBuffer struct {
	b    []byte // from alloc
	read int
	next *linkBuffer
}
