package flowctl

import (
	"errors"
	"io"
	"sync"
)

var (
	ErrClosedBuffer = errors.New("buffer was closed")
)

type RxStatus struct {
	WindowMax      int32
	WindowRemained int32
	Size           int32
}

type RxBuffer struct {
	m sync.Mutex
	c sync.Cond

	closed   bool
	closeErr error

	stat RxStatus

	head *memChunk
	tail *memChunk
}

func NewRxBuffer(initWindow int32) *RxBuffer {
	l := new(RxBuffer)
	l.c.L = &l.m
	l.stat.WindowMax = initWindow
	l.stat.WindowRemained = initWindow
	return l
}

func (l *RxBuffer) isEmpty() bool {
	return l.head == nil || l.head.rp == l.head.wp
}

// bytes returns a buffer chunk that is available to read. It blocks until more data is available.
// Once the buffer is consumed, user needs to call l.skip().
// The returned buffer is valid until next l.bytes() call.
func (l *RxBuffer) bytes() ([]byte, error) {
	var releasedC *memChunk
	defer func() {
		if releasedC != nil {
			releaseMemChunk(releasedC)
		}
	}()

	l.m.Lock()
	if l.head != nil {
		// EOF of this chunk. Advance to next chunk (maybe nil).
		if int(l.head.rp) >= len(l.head.b) {
			releasedC = l.head
			l.head = releasedC.next
			if l.head == nil { // Empty list.
				l.tail = nil
			}
		}
	}

	for {
		if l.isEmpty() {
			if l.closed {
				err := l.closeErr
				l.m.Unlock()
				return nil, err
			}
			l.c.Wait()
		} else {
			buf := l.head.b[l.head.rp:l.head.wp]
			l.m.Unlock()
			return buf, nil
		}
	}
}

// skip moves the read pointer for the next l.bytes() call.
func (l *RxBuffer) skip(n int) RxStatus {
	l.m.Lock()
	defer l.m.Unlock()
	l.head.rp += n
	l.stat.Size -= int32(n)
	stat := l.stat
	return stat
}

func (l *RxBuffer) Read(p []byte) (stat RxStatus, n int, err error) {
	b, err := l.bytes()
	if err != nil {
		return RxStatus{}, 0, err
	}
	n = copy(p, b)
	stat = l.skip(n)
	return stat, n, err
}

// Write one chunk to w. If there is nothing to write, it blocks until more
// data is available or l.CloseRead() was called.
func (l *RxBuffer) WriteChunk(w io.Writer) (stat RxStatus, n int, err error) {
	b, err := l.bytes()
	if err != nil {
		return RxStatus{}, 0, err
	}
	n, err = w.Write(b)
	stat = l.skip(n)
	return stat, n, err
}

// Call r.Read() with inner buffer chunk. If limit > 0, then the read buffer will not
// larger than the limit.
func (l *RxBuffer) ReadChunk(r io.Reader, limit int) (stat RxStatus, n int, err error) {
	l.m.Lock()
	if l.closed {
		stat = l.stat
		l.m.Unlock()
		return stat, 0, ErrClosedBuffer
	}

	// Find a chunk buffer for read.
	c := l.tail
	if c == nil { // Empty link list.
		c = getMemChunk()
		l.head = c
		l.tail = c
	}
	if int(c.wp) >= len(c.b) { // This chunk is full.
		c = getMemChunk()
		l.tail.next = c
		l.tail = c
	}
	l.m.Unlock()

	buf := c.b[c.wp:]
	if limit > 0 && limit < len(buf) {
		buf = buf[:limit]
	}
	n, err = r.Read(buf)

	l.m.Lock()
	if n > 0 {
		c.wp += n
		l.stat.Size += int32(n)
		l.stat.WindowRemained -= int32(n)
		l.c.Signal()
	}
	stat = l.stat
	l.m.Unlock()
	if stat.WindowRemained < 0 {
		return stat, n, ErrFlowControlUnderflow
	}
	return stat, n, err
}

func (l *RxBuffer) Status() RxStatus {
	l.m.Lock()
	defer l.m.Unlock()
	return l.stat
}

func (l *RxBuffer) IncWindow(d int32) RxStatus {
	l.m.Lock()
	defer l.m.Unlock()
	l.stat.WindowRemained += d
	return l.stat
}

func (l *RxBuffer) SetMaxWindow(d int32) RxStatus {
	l.m.Lock()
	defer l.m.Unlock()
	l.stat.WindowMax = d
	return l.stat
}

// Close this buffer. Unblock any blocked ops.
// The [err] will pass to all blocked called.
// If [err] is nil, the default error is [io.EOF]
func (l *RxBuffer) CloseWithErr(err error) {
	if err == nil {
		err = io.EOF
	}

	l.m.Lock()
	defer l.m.Unlock()

	if l.closed {
		return
	}
	l.closed = true
	l.closeErr = err
	l.c.Broadcast()
}

type memChunk struct {
	b    [16 * 1024]byte
	rp   int
	wp   int
	next *memChunk
}

var memChunkPool = sync.Pool{New: func() any { return new(memChunk) }}

func getMemChunk() *memChunk {
	return memChunkPool.Get().(*memChunk)
}

func releaseMemChunk(c *memChunk) {
	c.rp, c.wp = 0, 0
	c.next = nil
	memChunkPool.Put(c)
}
