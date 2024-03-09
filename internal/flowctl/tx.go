package flowctl

import (
	"io"
	"sync"
)

type TxCtrl struct {
	m        sync.Mutex
	c        sync.Cond
	window   int32
	closed   bool
	closeErr error
}

func NewTxCtrl(initWindow int32) *TxCtrl {
	wc := &TxCtrl{
		window: initWindow,
	}
	wc.c.L = &wc.m
	return wc
}

// Returns
// ErrFlowWindowOverflow if window was overflowed.
// ErrNegativeWindowUpdate if i < 0.
func (c *TxCtrl) IncWindow(i int32) error {
	if i < 0 {
		return ErrNegativeWindowUpdate
	}
	c.m.Lock()
	nw := c.window + i
	if nw < 0 {
		c.m.Unlock()
		return ErrFlowWindowIncOverflow
	}
	c.window = nw
	c.c.Signal()
	c.m.Unlock()
	return nil
}

// ConsumeWindow tries to consume n bytes window. It will consume
// less than n window size if the available size is small than n.
// It blocks if window size is zero.
// If c was closed, it returns with an error.
// Concurrent safe.
func (c *TxCtrl) ConsumeWindow(n int32) (int32, error) {
	c.m.Lock()
	defer c.m.Unlock()
	awakened := false
	for {
		if c.closed {
			err := c.closeErr
			return 0, err
		}

		d := c.tryConsumeLocked(n)
		if d > 0 {
			if awakened {
				// Chain wake up other waiting goroutines.
				c.c.Signal()
			}
			return d, nil
		}
		c.c.Wait()
		awakened = true
	}
}

func (c *TxCtrl) tryConsumeLocked(s int32) int32 {
	d := min(c.window, s)
	c.window -= d
	return d
}

// If err == nil, the close err will be io.EOF.
func (c *TxCtrl) CloseWithErr(err error) {
	if err == nil {
		err = io.EOF
	}

	c.m.Lock()
	defer c.m.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.closeErr = err
	c.c.Broadcast()
}
