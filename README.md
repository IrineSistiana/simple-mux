# simple-mux

[![Go Reference](https://pkg.go.dev/badge/github.com/IrineSistiana/simple-mux.svg)](https://pkg.go.dev/github.com/IrineSistiana/simple-mux)

A simple connection multiplexing library for Golang.

- Focus on performance. 
- Bi-directional streams. Streams can also be opened from server side to client.
- Stream level flow control. Built-in rx buffer.
- Session level health check and keepalive.

## Example

```go
package main

import (
	"fmt"
	"io"
	"net"

	smux "github.com/IrineSistiana/simple-mux"
)

func main() {
	panicIfErr := func(err error) {
		if err != nil {
			panic(err.Error())
		}
	}

	clientConn, serverConn := net.Pipe()
	clientSession := smux.NewSession(clientConn, smux.Opts{})
	serverSession := smux.NewSession(serverConn, smux.Opts{AllowAccept: true})

	go func() {
		stream, err := clientSession.OpenStream()
		panicIfErr(err)
		defer stream.Close()

		_, err = stream.Write([]byte("hello world"))
		panicIfErr(err)
	}()

	clientStream, err := serverSession.Accept()
	panicIfErr(err)

	b, err := io.ReadAll(clientStream)
	panicIfErr(err)
	
	fmt.Printf("received msg from client: %s\n", b)
}
```

## Benchmark

Transfer data concurrently through multiple streams over one tcp loopback connection. 

```text
Benchmark_Mux/1_streams-8                 335876             16582 ns/op              1885 Mb/s        0 B/op          0 allocs/op
Benchmark_Mux/8_streams-8                 250275             22085 ns/op              1415 Mb/s        7 B/op          0 allocs/op
Benchmark_Mux/64_streams-8                276235             21512 ns/op              1453 Mb/s        2 B/op          0 allocs/op
Benchmark_Mux/512_streams-8               237309             21215 ns/op              1473 Mb/s        2 B/op          0 allocs/op
Benchmark_Mux/2048_streams-8              266313             21881 ns/op              1428 Mb/s       14 B/op          0 allocs/op
Benchmark_Mux/8196_streams-8              252049             22783 ns/op              1372 Mb/s       39 B/op          0 allocs/op

```

Baseline: Transfer data directly through one tcp loopback connection. 

```text
Benchmark_TCP-8                           395679             14765 ns/op              2116 Mb/s        0 B/op          0 allocs/op
```
