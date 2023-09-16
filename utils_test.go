package mux

import (
	"testing"
	"time"
)

func Benchmark_IdleTimer(b *testing.B) {
	t := newIdleTimer(time.Second, func() {})
	for i := 0; i < b.N; i++ {
		t.reset()
	}
}
