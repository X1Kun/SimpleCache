package app

import (
	"net"
	"net/http"
	"testing"
)

func TestBindServersClosesEarlierListenersOnPartialFailure(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	_ = reservation.Close()
	listeners, err := bindServers([]*http.Server{{Addr: address}, {Addr: address}})
	if err == nil || listeners != nil {
		for _, listener := range listeners {
			_ = listener.Close()
		}
		t.Fatal("duplicate binding unexpectedly succeeded")
	}
	reopened, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal("partial binding leaked the first listener", err)
	}
	_ = reopened.Close()
}
