package mux

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pipeSessions(clientOpts, serverOpts Opts) (clientSession *Session, serverSession *Session) {
	c, s := net.Pipe()
	return NewSession(c, clientOpts), NewSession(s, serverOpts)
}

func runDummyDiscardAllSession(session *Session) {
	for {
		sm, err := session.Accept()
		if err != nil {
			return
		}
		go func() {
			_, _ = io.Copy(io.Discard, sm)
		}()
	}
}

// Send data from client to server.
func Test_MuxSingleSteamIO(t *testing.T) {
	r := require.New(t)

	clientSession, serverSession := pipeSessions(Opts{}, Opts{AllowAccept: true})

	data := make([]byte, 128*1024)
	rand.Read(data)

	go func() {
		stream, err := serverSession.Accept()
		r.NoError(err)
		defer stream.Close()

		n, err := stream.Write(data)
		r.Equal(len(data), n)
		r.NoError(err)
	}()

	stream, err := clientSession.OpenStream()
	r.NoError(err)
	defer stream.Close()

	buf := make([]byte, len(data))
	_, err = io.ReadFull(stream, buf)
	r.NoError(err)
	r.Equal(data, buf)

	select {
	case <-time.After(time.Second):
		r.Fail("stream should be closed")
	case <-stream.Context().Done():
		r.ErrorIs(context.Cause(stream.Context()), io.EOF)
	}
}

// Echo data between client and server using multiple streams concurrently.
func Test_Mux_MultiStreamEcho(t *testing.T) {
	a := assert.New(t)

	junk := make([]byte, 10*1024*1024)
	rand.Read(junk)

	c, s := pipeSessions(Opts{}, Opts{AllowAccept: true})

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
				defer sm.Close()

				// trigger a window update
				sm.SetRxWindowSize(1024 * 1024)
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
			}()

			sm, err := s.Accept()
			if err != nil {
				t.Error(err)
				return
			}
			defer sm.Close()

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

func Test_Mux_IdleTimeout(t *testing.T) {
	// Once there is no active stream in the session, the idle timer will fire after idle timeout.
	t.Run("reused_session", func(t *testing.T) {
		r := require.New(t)
		clientSession, serverSession := pipeSessions(Opts{IdleTimeout: time.Millisecond * 200}, Opts{AllowAccept: true})
		defer func() {
			clientSession.Close()
			serverSession.Close()
		}()
		go runDummyDiscardAllSession(serverSession)

		var streams []*Stream
		for i := 0; i < InitialStreamQuota; i++ {
			stream, err := clientSession.OpenStream()
			r.NoError(err)
			streams = append(streams, stream)
		}
		for _, stream := range streams {
			stream.Close()
		}

		select {
		case <-time.After(time.Second):
			r.Fail("no idle timeout")
		case <-clientSession.Context().Done():
			r.ErrorIs(context.Cause(clientSession.Context()), ErrIdleTimedOut)
		}
	})

	// Same for new created session.
	t.Run("new_session", func(t *testing.T) {
		r := require.New(t)
		clientSession, serverSession := pipeSessions(Opts{IdleTimeout: time.Millisecond * 200}, Opts{AllowAccept: true})
		defer func() {
			clientSession.Close()
			serverSession.Close()
		}()
		go runDummyDiscardAllSession(serverSession)

		select {
		case <-time.After(time.Second):
			r.Fail("no idle timeout")
		case <-clientSession.Context().Done():
			r.ErrorIs(context.Cause(clientSession.Context()), ErrIdleTimedOut)
		}
	})
}

// If keepalive was set, if no data read after KeepaliveInterval, a
// ping will be sent.
// If no pong was received after KeepaliveTimeout, the session will be
// closed with ErrKeepaliveTimedOut.
func Test_Mux_PingTimeout(t *testing.T) {
	r := require.New(t)
	clientSession, serverSession := pipeSessions(
		Opts{
			KeepaliveInterval: time.Millisecond * 100,
			KeepaliveTimeout:  time.Millisecond * 100,
		},
		Opts{
			AllowAccept: true,
			testNoPong:  true,
		},
	)

	go runDummyDiscardAllSession(serverSession)
	defer func() {
		clientSession.Close()
		serverSession.Close()
	}()

	select {
	case <-time.After(time.Second):
		r.Fail("no keepalive timeout")
	case <-clientSession.Context().Done():
		r.ErrorIs(context.Cause(clientSession.Context()), ErrKeepaliveTimedOut)
	}
}

func Test_Mux_PeerClosing(t *testing.T) {
	r := require.New(t)
	clientSession, serverSession := pipeSessions(Opts{}, Opts{AllowAccept: true})
	go runDummyDiscardAllSession(serverSession)
	defer func() {
		clientSession.Close()
		serverSession.Close()
	}()

	clientStream, err := clientSession.OpenStream()
	r.NoError(err)
	defer clientStream.Close()

	serverSession.Close()
	// server sends go away frame to client
	// client session closed
	// client streams closed

	select {
	case <-time.After(time.Second):
		r.Fail("session does not close")
	case <-clientSession.Context().Done():
		r.ErrorIs(context.Cause(clientSession.Context()), io.EOF)
	}

	select {
	case <-time.After(time.Second):
		r.Fail("stream does not close")
	case <-clientStream.Context().Done():
		r.ErrorIs(context.Cause(clientStream.Context()), io.EOF)
	}
}

// Session cannot open more stream if its quota is zero. OpenStream() should fail.
func Test_Mux_StreamQuotaLocalLimit(t *testing.T) {
	r := require.New(t)
	clientSession, serverSession := pipeSessions(Opts{}, Opts{AllowAccept: true, noStreamQuotaUpdate: true})
	go runDummyDiscardAllSession(serverSession)
	defer func() {
		clientSession.Close()
		serverSession.Close()
	}()

	clientQuota := clientSession.Status().LocalStreamQuota

	for i := int32(0); i < clientQuota; i++ {
		stream, err := clientSession.OpenStream()
		r.NoError(err)
		stream.Close()
	}

	_, err := clientSession.OpenStream()
	r.ErrorIs(err, ErrStreamQuotaLimit)
}

// If client violates the stream quota, the server should close the connection.
func Test_Mux_StreamQuotaPeerViolation(t *testing.T) {
	r := require.New(t)
	clientSession, serverSession := pipeSessions(
		Opts{noLocalStreamQuotaLimit: true},
		Opts{AllowAccept: true, noStreamQuotaUpdate: true},
	)
	go runDummyDiscardAllSession(serverSession)
	defer func() {
		clientSession.Close()
		serverSession.Close()
	}()

	clientQuota := clientSession.Status().LocalStreamQuota

	for i := int32(0); i < clientQuota; i++ {
		stream, err := clientSession.OpenStream()
		r.NoError(err)
		stream.Close()
	}

	_, _ = clientSession.OpenStream()

	select {
	case <-time.After(time.Second):
		r.Fail("stream does not close")
	case <-serverSession.Context().Done():
		r.ErrorIs(context.Cause(serverSession.Context()), errStreamQuotaViolation)
	}
}

// If WriteTimeout is set and c supposes ddl. The connection should be closed after
// write timeout.
func Test_Mux_WriteTimeout(t *testing.T) {
	r := require.New(t)
	clientConn, _ := net.Pipe() 
	clientSession := NewSession(clientConn, Opts{WriteTimeout: time.Millisecond * 100})
	defer clientSession.Close()

	go func() {
		clientSession.Ping(context.Background())
	}()

	select {
	case <-time.After(time.Second):
		r.Fail("session should have write timeout")
	case <-clientSession.Context().Done():
	}
}
