package mux

import (
	"github.com/urlesistiana/alloc-go"
	"sync"
)

// buffer from alloc and should be released manually
type allocBuffer struct {
	b []byte
}

func (b *allocBuffer) len() int {
	return len(b.b)
}

func getBuffer(i int) allocBuffer {
	return allocBuffer{b: alloc.Get(i)}
}

func releaseBuffer(b allocBuffer) {
	alloc.Release(b.b)
}

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

func getLinkBuffer() *linkBuffer {
	return linkBufferPool.Get().(*linkBuffer)
}

func releaseLinkBuffer(lb *linkBuffer) {
	alloc.Release(lb.b)
	lb.b = nil
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
