package mux

import (
	"bytes"
	"github.com/stretchr/testify/assert"
	"io"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"
)

func pipe() (c *Session, s *Session, err error) {
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

	c = NewSession(cr.c, Opts{})
	s = NewSession(sr.c, Opts{
		AllowAccept: true,
	})
	return c, s, nil
}

func Test_Mux_IO(t *testing.T) {
	a := assert.New(t)

	junk := make([]byte, 10*1024*1024)
	rand.Read(junk)

	c, s, err := pipe()
	if err != nil {
		t.Fatal(err)
	}

	wg := new(sync.WaitGroup)
	for i := 0; i < 16; i++ { // concurrent 16 streams
		wg.Add(1)
		go func() {
			defer wg.Done()
			go func() {
				sm, err := c.OpenStream()
				if err != nil {
					t.Error(err)
					return
				}
				sm.SetRxWindowSize(1024 * 1024) // trigger a window update
				sm.SetRxWindowSize(64 * 1024)

				// test read
				buf := make([]byte, len(junk))
				_, err = io.ReadFull(sm, buf)
				if err != nil {
					t.Error(err)
					return
				}
				a.Equal(junk, buf)

				_, err = sm.Write(junk)
				if err != nil {
					t.Error(err)
					return
				}
				_ = sm.Close()
			}()

			sm, err := s.Accept()
			if err != nil {
				t.Error(err)
				return
			}
			_, err = sm.Write(junk)
			if err != nil {
				t.Error(err)
				return
			}

			// test WriteTo
			buf := new(bytes.Buffer)
			_, err = sm.WriteTo(buf)
			got := buf.Bytes()
			if err != nil && err != io.EOF {
				t.Error(err)
				return
			}
			a.Equal(junk, got)
		}()
	}
	wg.Wait()
}

func Test_Mux_Ping_Idle_Timeout(t *testing.T) {
	c1, c2 := net.Pipe()
	sess := NewSession(c1, Opts{
		AllowAccept:  false,
		PingInterval: time.Millisecond * 50,
		IdleTimeout:  time.Millisecond * 200,
	})

	sess2 := NewSession(c2, Opts{
		AllowAccept:  true,
		PingInterval: time.Millisecond * 50,
		IdleTimeout:  time.Hour,
	})
	go func() {
		for {
			sm, err := sess2.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(io.Discard, sm)
			}()
		}
	}()
	defer func() {
		_ = sess.Close()
		_ = sess2.Close()
	}()

	sm, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sm.Write(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond * 500)
	if err := sess.CloseErr(); err != ErrIdleTimeout {
		t.Fatalf("sess should be closed due to idle timed out, got err %v", err)
	}
}
