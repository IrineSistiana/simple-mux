package mux

import (
	"bytes"
	"context"
	"github.com/stretchr/testify/assert"
	"github.com/urlesistiana/alloc-go"
	"io"
	"sync"
	"testing"
	"time"
)

func Test_windowControl(t *testing.T) {
	t.Run("block", func(t *testing.T) {
		a := assert.New(t)
		wc := newOutflowControl()
		c := make(chan struct{})
		close(c)
		consumed, ok := wc.consume(MinWindow-1, c)
		a.EqualValues(consumed, MinWindow-1)
		a.True(ok)
		consumed, ok = wc.consume(2, c)
		a.EqualValues(consumed, 1)
		a.True(ok)
		consumed, ok = wc.consume(2, c)
		a.EqualValues(consumed, 0)
		a.False(ok)

		go func() {
			wc.inc(1)
		}()
		consumed, ok = wc.consume(2, nil)
		a.EqualValues(consumed, 1)
		a.True(ok)
	})

	t.Run("update notify", func(t *testing.T) {
		a := assert.New(t)
		wc := newOutflowControl()
		wc.consume(MinWindow, nil)
		res := make([]bool, 64)
		wg := new(sync.WaitGroup)
		for i := range res {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				consumed, ok := wc.consume(1, nil)
				if consumed == 1 && ok {
					res[i] = true
				}
			}()
		}
		wc.inc(uint32(len(res)))
		wg.Wait()
		for _, ok := range res {
			a.True(ok)
		}
	})
}

func Test_readBuffer(t *testing.T) {
	t.Run("io", func(t *testing.T) {
		a := assert.New(t)
		b := newRxBuffer(128)
		writeData := make([]byte, 0)
		go func() {
			for i := 0; i < 128; i++ {
				p := alloc.Get(1)
				p[0] = byte(i)
				writeData = append(writeData, p...)
				overflowed, closed := b.pushBuffer(allocBuffer{p})
				if !a.False(overflowed) || !a.False(closed) {
					return
				}
			}
		}()

		rb := make([]byte, 128)
		rbn := 0
		var reader readFunc = func(p []byte) (n int, err error) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			n, windowC, ok := b.read(p, ctx.Done())
			rbn += n
			if !ok {
				err = ctx.Err()
			}
			a.Equal(uint32(len(rb)-rbn), windowC)
			return n, err
		}
		_, err := io.ReadFull(reader, rb)
		a.Nil(err)
		a.True(bytes.Equal(writeData, rb))
	})

	t.Run("overflow and closed", func(t *testing.T) {
		a := assert.New(t)
		b := newRxBuffer(16)
		overflowed, closed := b.pushBuffer(getBuffer(16))
		a.False(overflowed)
		a.False(closed)
		overflowed, closed = b.pushBuffer(getBuffer(1))
		a.True(overflowed)
		a.False(closed)

		b.close()
		overflowed, closed = b.pushBuffer(getBuffer(1))
		a.False(overflowed)
		a.True(closed)
	})
}
