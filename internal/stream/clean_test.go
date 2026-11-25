package stream

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// TestCleanEndReset: quitting ATAK resets the connection, which counts as
// signing off; a stream that goes silent does not.
func TestCleanEndReset(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return
		}
		c.(*net.TCPConn).SetLinger(0) // close with a reset
		c.Close()
	}()
	srv, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, rerr := srv.Read(make([]byte, 16))
	if rerr == nil || errors.Is(rerr, os.ErrDeadlineExceeded) {
		t.Fatalf("read after reset = %v", rerr)
	}
	if !cleanEnd(rerr) {
		t.Errorf("reset (%v) not counted as signing off", rerr)
	}
	if !cleanEnd(io.EOF) {
		t.Error("EOF not clean")
	}
	if cleanEnd(&net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}) {
		t.Error("a silent stream counted as signing off")
	}
}
