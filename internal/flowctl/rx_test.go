package flowctl

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
)

func Test_RxBuffer(t *testing.T) {
	const dataSize = 16 * 1024 * 1024

	l := NewRxBuffer(dataSize)

	data := make([]byte, dataSize)
	rand.Reader.Read(data)

	go func() {
		defer l.CloseWithErr(nil)
		r := bytes.NewBuffer(data)
		var totalRead int
		var s RxStatus
		for {
			stat, n, err := l.ReadChunk(r, 0)
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				t.Error(err)
				return
			}
			totalRead += n
			s = stat
		}
		if totalRead != len(data) {
			t.Error("partial read")
			return
		}
		if s.WindowRemained != 0 {
			t.Error("invalid window remained")
			return
		}
	}()

	buf := new(bytes.Buffer)

	totalWrite := 0
	for {
		_, n, err := l.WriteChunk(buf)
		totalWrite += n
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
	}

	if totalWrite != len(data) {
		t.Fatalf("partial write")
	}

	if !bytes.Equal(buf.Bytes(), data) {
		t.Fatal("broken data")
	}
}
