package mux

import (
	"sync"
	"sync/atomic"
	"time"

	bp "github.com/IrineSistiana/go-bytes-pool"
)

var bytesPool = bp.NewPool(24)

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

type idleControl struct {
	d time.Duration

	latestResetTimeMs atomic.Int64
	m                 sync.Mutex
	fired             bool
	t                 *time.Timer
}

func newIdleTimer(d time.Duration, closeFn func()) *idleControl {
	return &idleControl{
		d: d,
		t: time.AfterFunc(d, closeFn),
	}
}

func (t *idleControl) reset() {
	const (
		updateInterval = int64(time.Millisecond * 10)
	)

	nowMs := time.Now().UnixMilli()
	if nowMs-t.latestResetTimeMs.Load() < updateInterval {
		return
	}
	t.latestResetTimeMs.Store(nowMs)

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

func (t *idleControl) stop() {
	t.m.Lock()
	defer t.m.Unlock()

	t.t.Stop()
	t.fired = true
}
