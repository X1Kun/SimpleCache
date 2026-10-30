package app

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDataServerTimesOutBlockedResponse(t *testing.T) {
	finished := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := make([]byte, 1<<20)
		for i := 0; i < 64; i++ {
			if _, err := w.Write(chunk); err != nil {
				finished <- err
				return
			}
		}
		finished <- nil
	})
	server := httptest.NewUnstartedServer(handler)
	server.Config = newDataServer("", handler)
	server.Start()
	defer server.Close()
	conn, err := net.DialTCP("tcp", nil, server.Listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatal("expected response write timeout", err)
		}
	case <-time.After(responseWriteTimeout + 3*time.Second):
		t.Fatal("blocked response outlived write deadline")
	}
}
