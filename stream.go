package mux

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

const (
	MinWindow = 64*1024 - 1
	MaxWindow = 1<<31 - 1
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

func (s *Stream) WriteTo(w io.Writer) (int64, error) {
	var n int64
	for {
		f, ok := s.rb.popHeadFrame(s.closeNotify)
		if !ok {
			return n, s.closeErr
		}
		nw, err := w.Write(f)
		n += int64(nw)
		remainWindow := s.rb.consumeHeadFrame(nw)
		s.adjustWindow(s.rxWindow.Load(), remainWindow)
		if err != nil {
			return n, err
		}
	}
}

// SetRxWindowSize sets the stream rx windows size.
// If n is invalid, the closest limit will be used.
func (s *Stream) SetRxWindowSize(n uint32) {
	s.rxWindow.Store(validWindowSize(n))
	s.adjustWindow(s.rxWindow.Load(), s.rb.currentWindow())
}

func (s *Stream) read(p []byte) (n int, err error) {
	n, currentWindow, ok := s.rb.read(p, s.closeNotify)
	if !ok {
		return n, s.closeErr
	}
	s.adjustWindow(s.rxWindow.Load(), currentWindow)
	return n, err
}

// adjust local and peer window
func (s *Stream) adjustWindow(target, current uint32) {
	minUpdate := target / 8 // avoid dense ack

	// update peer window
	if ackPending := target - current; target > current && // avoid negative
		ackPending >= minUpdate {
		s.rb.windowInc(ackPending)
		s.incPeerWindow(ackPending)
	}
}

func (s *Stream) incPeerWindow(i uint32) {
	// ignore write error.
	// if session has a write error, it will close this stream anyway.
	select {
	case s.sess.writeOpChan <- writeFrameOp{b: packWindowUpdateFrame(s.id, i).b, releaseB: true}:
	case <-s.closeNotify:
	}
}

// Write implements io.Writer.
func (s *Stream) Write(p []byte) (n int, err error) {
	return s.write(p)
}

func (s *Stream) write(p []byte) (n int, err error) {
	for n < len(p) {
		nw, err := s.writeDataFrameToSess(p[n:])
		n += nw
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// write at most maxPayloadLength bytes of b to session.
func (s *Stream) writeDataFrameToSess(b []byte) (int, error) {
	if len(b) > maxPayloadLength {
		b = b[:maxPayloadLength]
	}

	// acquire window
	ready, ok := s.flowControl.consume(uint32(len(b)), s.closeNotify)
	if !ok { // stream closed
		return 0, s.closeErr
	}
	b = b[:ready]

	rc := getWriteResChan()
	f := packDataFrame(s.id, b)
	defer releaseWriteResChan(rc)
	defer releaseBuffer(f)
	select {
	case s.sess.writeOpChan <- writeFrameOp{b: f.b, rc: rc}:
		res := <-rc
		n := res.n - 7 // data frame has 7 bytes header
		if n < 0 {
			n = 0
		}
		return n, res.err
	case <-s.closeNotify:
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
	m         sync.Mutex
	window    uint32
	incSignal chan struct{}
}

func newOutflowControl() *outflowControl {
	wc := &outflowControl{window: MinWindow, incSignal: make(chan struct{}, 1)}
	return wc
}

func (wc *outflowControl) inc(i uint32) bool {
	if i > MaxWindow { // overflowed already
		return false
	}

	wc.m.Lock()
	nw := wc.window + i
	if nw > MaxWindow { // overflowed
		wc.m.Unlock()
		return false
	}
	wc.window = nw
	wc.m.Unlock()
	wc.sendIncSignal()
	return true
}

func (wc *outflowControl) sendIncSignal() {
	select {
	case wc.incSignal <- struct{}{}:
	default:
	}
}

// consume tries to consume s window or wait until canceled if no window was available.
// It's concurrent safe. Multiple consume calls can be waiting on the same outflowControl.
func (wc *outflowControl) consume(s uint32, cancel <-chan struct{}) (uint32, bool) {
	wakenUp := false
consume:
	wc.m.Lock()
	if wc.window >= s { // window is large enough for s.
		wc.window -= s
		hasMoreWindow := wc.window > 0
		wc.m.Unlock()
		if wakenUp && hasMoreWindow {
			// There may have more waiting goroutines,
			// send another signal to wake up another goroutine.
			// (chain wakeup)
			wc.sendIncSignal()
		}
		return s, true
	}
	if wc.window != 0 && wc.window < s { // window is not large enough and is depleted by s
		consumed := wc.window
		wc.window = 0
		wc.m.Unlock()
		return consumed, true
	}
	wc.m.Unlock()

	// window is 0. wait for inc()
	select {
	case <-wc.incSignal:
		// Note: incSignal has buffer, first wakeup
		// may not have window to consume. (old signal)
		wakenUp = true
		goto consume
	case <-cancel:
		return 0, false
	}
}

type rxBuffer struct {
	pushSignal chan struct{}

	m      sync.Mutex
	window uint32
	size   int
	head   *linkBuffer
	tail   *linkBuffer
	closed bool
}

func newRxBuffer(window uint32) *rxBuffer {
	return &rxBuffer{
		window:     window,
		pushSignal: make(chan struct{}, 1),
	}
}

func (r *rxBuffer) adjustHeadLocked() {
	for {
		lb := r.head
		if lb == nil {
			// no frame
			return
		}
		if lb.read == len(lb.b) {
			// this frame is depleted
			next := lb.next
			releaseLinkBuffer(lb)
			if next == nil { // end of buffer
				r.head = nil
				r.tail = nil
				return
			}
			r.head = next
			continue // check next frame
		}
		return
	}
}

func (r *rxBuffer) read(p []byte, cancel <-chan struct{}) (n int, window uint32, ok bool) {
read:
	// read from buffers
	r.m.Lock()
	for {
		r.adjustHeadLocked()
		if r.head == nil {
			break // empty buffer
		}

		f := r.head
		nr := copy(p[n:], f.b[f.read:])
		n += nr
		f.read += nr
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
	case <-r.pushSignal:
		// Note: pushSignal has buffer, first signal
		// may not be old. It's ok.
		goto read
	case <-cancel:
		return 0, 0, false
	}
}

func (r *rxBuffer) popHeadFrame(cancel <-chan struct{}) ([]byte, bool) {
read:
	// read from buffers
	r.m.Lock()
	r.adjustHeadLocked()
	if f := r.head; f != nil {
		b := f.b[f.read:]
		r.m.Unlock()
		return b, true
	}
	r.m.Unlock()

	// empty buffer, wait
	select {
	case <-r.pushSignal:
		goto read
	case <-cancel:
		return nil, false
	}
}

// mark how many bytes were consumed in the head frame.
// Should be used with popHeadFrame.
func (r *rxBuffer) consumeHeadFrame(i int) uint32 {
	r.m.Lock()
	r.head.read += i
	r.size -= i
	r.window -= uint32(i)
	w := r.window
	r.m.Unlock()
	return w
}

func (r *rxBuffer) windowInc(i uint32) {
	r.m.Lock()
	defer r.m.Unlock()
	r.window += i
}

func (r *rxBuffer) currentWindow() uint32 {
	r.m.Lock()
	defer r.m.Unlock()
	return r.window
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
	r.m.Unlock()

	select {
	case r.pushSignal <- struct{}{}:
	default:
	}
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
