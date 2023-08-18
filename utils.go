package mux

import (
	"sync"
	"time"

	bytesPool "github.com/IrineSistiana/go-bytes-pool"
)

var writeResChanPool = sync.Pool{New: func() any {
	return make(chan writeRes, 1)
}}

func getWriteResChan() chan writeRes {
	return writeResChanPool.Get().(chan writeRes)
}

func releaseWriteResChan(c chan writeRes) {
	select {
	case <-c:
	default:
	}
	writeResChanPool.Put(c)
}

var linkBufferPool = sync.Pool{New: func() any {
	return new(linkBuffer)
}}

type linkBuffer struct {
	bp   *[]byte
	read int
	next *linkBuffer
}

func getLinkBuffer() *linkBuffer {
	return linkBufferPool.Get().(*linkBuffer)
}

func releaseLinkBuffer(lb *linkBuffer) {
	bytesPool.Release(lb.bp)
	lb.bp = nil
	lb.read = 0
	lb.next = nil
	linkBufferPool.Put(lb)
}

func validWindowSize(i uint32) uint32 {
	switch {
	case i < MinWindow:
		i = MinWindow
	case i > MaxWindow:
		i = MaxWindow
	}
	return i
}

type idleTimer struct {
	d time.Duration

	m     sync.Mutex
	fired bool
	t     *time.Timer
}

func newIdleTimer(d time.Duration, f func()) *idleTimer {
	return &idleTimer{
		d: d,
		t: time.AfterFunc(d, f),
	}
}

func (t *idleTimer) reset() {
	t.m.Lock()
	defer t.m.Unlock()

	if t.fired {
		return
	}

	if !t.t.Reset(t.d) { // timer was fired, but we re-activated it.
		t.t.Stop()
		t.fired = true
	}
}

func (t *idleTimer) stop() {
	t.m.Lock()
	defer t.m.Unlock()

	t.t.Stop()
	t.fired = true
}
