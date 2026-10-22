package peer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	pb "github.com/X1Kun/simpleCache/geecache-engine/internal/peer/peerpb"
	"google.golang.org/protobuf/proto"
)

func TestPeerTTLDoesNotRestartOwnersLifetime(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	body, _ := proto.Marshal(&pb.Response{Value: []byte("value")})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(TTLHeader, strconv.FormatInt(int64(time.Until(deadline)), 10))
		_, _ = w.Write(body)
	}))
	defer server.Close()
	value, err := clientFor(server).GetWithExpiry(context.Background(), "products", "key")
	if err != nil || value.ExpiresAt.IsZero() || value.ExpiresAt.After(deadline) {
		t.Fatalf("extended expiry: %+v %v", value, err)
	}
}

func TestLegacyAndInvalidTTLHeaders(t *testing.T) {
	for _, header := range []string{"", "0", "-1", "invalid", "9223372036854775808"} {
		t.Run(header, func(t *testing.T) {
			body, _ := proto.Marshal(&pb.Response{Value: []byte("value")})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if header != "" {
					w.Header().Set(TTLHeader, header)
				}
				_, _ = w.Write(body)
			}))
			defer server.Close()
			value, err := clientFor(server).GetWithExpiry(context.Background(), "products", "key")
			if header == "" || header == "0" {
				if err != nil || !value.ExpiresAt.IsZero() || string(value.Value) != "value" {
					t.Fatal(value, err)
				}
			} else if err == nil {
				t.Fatal("invalid TTL accepted")
			}
		})
	}
}
