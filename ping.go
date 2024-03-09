package mux

import (
	"sync"
)

type pingCtl struct {
	m     sync.Mutex
	c     uint32
	pings map[uint32]chan struct{}
}

func newPingCtl() *pingCtl {
	return &pingCtl{
		pings: make(map[uint32]chan struct{}),
	}
}

func (c *pingCtl) Notify(id uint32) {
	c.m.Lock()
	nc := c.pings[id]
	if nc != nil {
		delete(c.pings, id)
	}
	c.m.Unlock()

	if nc != nil {
		close(nc)
	}
}

func (c *pingCtl) Reg() (id uint32, notify chan struct{}) {
	notify = make(chan struct{})
	c.m.Lock()
	c.c++
	id = c.c
	c.pings[id] = notify
	c.m.Unlock()
	return id, notify
}

func (c *pingCtl) Del(id uint32) {
	c.m.Lock()
	delete(c.pings, id)
	c.m.Unlock()
}
