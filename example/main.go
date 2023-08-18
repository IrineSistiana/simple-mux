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
