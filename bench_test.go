package mux

import (
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func Benchmark_Mux(b *testing.B) {
	blockSize := 32 * 1024
	wb := make([]byte, blockSize)
	c, s, err := tcpLoopbackPipe()
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		c.Close()
		s.Close()
	}()

	for _, concurrentStreams := range []int{1, 8, 64, 512, 2048, 8196} {
		concurrentStreams := concurrentStreams
		b.Run(fmt.Sprintf("%d_streams", concurrentStreams), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			start := time.Now()

			wg := new(sync.WaitGroup)
			var read atomic.Int64
			b.SetParallelism(2048)

			var n int = b.N
			var nm sync.Mutex

			next := func() bool {
				nm.Lock()
				defer nm.Unlock()
				if n <= 0 {
					return false
				}
				n--
				return true
			}

			writeFn := func() {
				stream, err := c.OpenStream()
				if err != nil {
					b.Error(err)
					return
				}
				for next() {
					if _, err := stream.Write(wb); err != nil {
						b.Error(err)
						return
					}
				}
				stream.Close()
			}

			readFn := func() {
				sm, err := s.Accept()
				if err != nil {
					b.Error(err)
					return
				}

				n, err := io.Copy(io.Discard, sm)
				if err != nil && err != io.EOF {
					b.Error(err)
				}
				read.Add(n)
			}

			for i := 0; i < concurrentStreams; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					writeFn()
				}()

				wg.Add(1)
				go func() {
					defer wg.Done()
					readFn()
				}()
			}
			wg.Wait()

			elapsed := time.Since(start)
			ioSum := read.Load()
			p := float64((ioSum)/1024/1024) / elapsed.Seconds()
			b.ReportMetric(p, "Mb/s")
		})
	}
}

func Benchmark_TCP(b *testing.B) {
	blockSize := 32 * 1024

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
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
	defer c.Close()

	rb := make([]byte, blockSize)
	start := time.Now()
	sum := 0

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n, err := c.Read(rb)
		sum += n
		if err != nil {
			b.Fatal(err)
		}
	}

	elapsed := time.Since(start)
	p := float64((sum)/1024/1024) / elapsed.Seconds()
	b.ReportMetric(p, "Mb/s")
}

func tcpLoopbackPipe() (clientSession *Session, serverSession *Session, err error) {
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return nil, nil, err
	}
	defer l.Close()

	type res struct {
		c   net.Conn
		err error
	}

	cDone := make(chan res)
	sDone := make(chan res)
	go func() {
		c, err := net.Dial("tcp", l.Addr().String())
		cDone <- res{c: c, err: err}
	}()
	go func() {
		c, err := l.AcceptTCP()
		sDone <- res{c: c, err: err}
	}()
	cr := <-cDone
	sr := <-sDone
	if cr.err != nil {
		return nil, nil, cr.err
	}
	if sr.err != nil {
		return nil, nil, sr.err
	}

	clientSession = NewSession(cr.c, Opts{
		StreamReceiveWindow: 1024 * 1024,
	})
	serverSession = NewSession(sr.c, Opts{
		StreamReceiveWindow:  1024 * 1024,
		MaxConcurrentStreams: 65535,
		AllowAccept:          true,
	})
	return clientSession, serverSession, nil
}
