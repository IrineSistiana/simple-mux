# simple-mux

[![Go Reference](https://pkg.go.dev/badge/github.com/IrineSistiana/simple-mux.svg)](https://pkg.go.dev/github.com/IrineSistiana/simple-mux)

A simple connection multiplexing library for Golang.

- KISS (keep it simple stupid).
- Almost Zero-alloc. Still needs to allocate buffers, but they will be reused.
- Small overhead. Data frame has 7 bytes header and can have 65535 bytes payload.
- Streams can be opened by either client or server.
- Builtin flow control and rx buffer for each stream. A slow io stream won't affect other streams in the same session.
- Builtin session ping&pong for health check and keepalive.

## Benchmark

```text
Benchmark_Mux_Concurrent_Write
Benchmark_Mux_Concurrent_Write-8                          754227              1371 ns/op             11400 Mb/s        0 B/op          0 allocs/op
Benchmark_Mux_Concurrent_IO_Through_Single_TCP
Benchmark_Mux_Concurrent_IO_Through_Single_TCP-8           63928             19053 ns/op               819.3 Mb/s              4 B/op          0 allocs/op
Benchmark_IO_Through_Single_TCP
Benchmark_IO_Through_Single_TCP-8                         148203              8247 ns/op              1894 Mb/s        0 B/op          0 allocs/op
```
