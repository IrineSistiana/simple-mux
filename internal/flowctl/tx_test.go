package flowctl

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func Test_OutflowControl(t *testing.T) {
	r := require.New(t)
	c := NewTxCtrl(0)

	go func() {
		for i := 0; i < 1000; i++ { // total 1000
			err := c.IncWindow(1)
			r.NoError(err)
		}
	}()

	total := 0
	for total < 1000 {
		consumed, err := c.ConsumeWindow(100)
		r.NoError(err)
		total += int(consumed)
	}

	if total != 1000 {
		t.Fatal("total window size is incorrect")
	}

	go func() {
		c.CloseWithErr(io.EOF)
	}()

	consumed, err := c.ConsumeWindow(1)
	r.Equal(int32(0), consumed)
	r.ErrorIs(err, io.EOF)
}
