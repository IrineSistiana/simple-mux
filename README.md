# simple-mux

[![Go Reference](https://pkg.go.dev/badge/github.com/IrineSistiana/simple-mux.svg)](https://pkg.go.dev/github.com/IrineSistiana/simple-mux)

A simple connection multiplexing library for Golang.

- KISS (keep it simple stupid).
- Almost Zero-alloc. Still needs to allocate buffers, but they will be reused.
- Small overhead. Data frame has 7 bytes header and can have 65535 bytes payload.
- Streams can be opened by either client or server.
- Builtin flow control and rx buffer for each stream. A slow io stream won't affect other streams in the same session.
- Builtin session ping&pong for health check and keepalive.

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

```text
# Sending data through 8 streams concurrently via a single TCP loopback connection.
Benchmark_Mux_Concurrent_IO_Through_Single_TCP-8           89846             11995 ns/op              1302 Mb/s        2 B/op          0 allocs/op

# Sending data directly through a TCP loopback connection.
Benchmark_IO_Through_Single_TCP-8                         125737              8676 ns/op              1799 Mb/s        0 B/op          0 allocs/op

# Single cpu
Benchmark_Mux_Concurrent_IO_Through_Single_TCP     47194             25205 ns/op               619.6 Mb/s              1 B/op             0 allocs/op
Benchmark_IO_Through_Single_TCP                    56523             18854 ns/op               828.6 Mb/s              0 B/op             0 allocs/op
PASS
```
