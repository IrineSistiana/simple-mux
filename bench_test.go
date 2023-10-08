package mux

import (
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func Benchmark_Mux_Concurrent_IO_Through_Single_TCP(b *testing.B) {
	blockSize := 16 * 1024

	c, s, err := tcpLoopbackPipe()
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()

	start := time.Now()
	var read atomic.Int64

	wb := make([]byte, blockSize)

	b.RunParallel(func(pb *testing.PB) {
		go func() {
			sm, err := c.OpenStream()
			if err != nil {
				b.Error(err)
				return
			}
			for pb.Next() {
				if _, err := sm.Write(wb); err != nil {
					b.Error(err)
					return
				}
			}
			_ = sm.Close()
		}()
		sm, err := s.Accept()
		if err != nil {
			b.Error(err)
			return
		}

		buf := bytesPool.Get(16 * 1024)
		defer bytesPool.Release(buf)

		n, err := io.CopyBuffer(io.Discard, sm, *buf)
		if err != nil && err != io.EOF {
			b.Error(err)
		}
		read.Add(n)
	})

	elapsed := time.Since(start)
	ioSum := read.Load()
	speedMs := float64((ioSum)/1024/1024) / elapsed.Seconds()
	b.ReportMetric(speedMs, "Mb/s")
}

func Benchmark_IO_Through_Single_TCP(b *testing.B) {
	blockSize := 16 * 1024

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	go func() {
		c, err := l.Accept()
		if err != nil {
			b.Error(err)
			return
		}
		wb := make([]byte, blockSize)
		for {
			if _, err := c.Write(wb); err != nil {
				return
			}
		}
	}()

	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		b.Fatal(err)
	}

	rb := make([]byte, blockSize)
	start := time.Now()
	var read atomic.Int64

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n, err := c.Read(rb)
		read.Add(int64(n))
		if err != nil {
			b.Fatal(err)
		}
	}

	elapsed := time.Since(start)
	ioSum := read.Load()
	speedMs := float64((ioSum)/1024/1024) / elapsed.Seconds()
	b.ReportMetric(speedMs, "Mb/s")
}
