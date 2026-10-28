package peer

import (
	"testing"
	"time"
)

func TestDirectoryTracksInvalidationMembersWithoutReadRing(t *testing.T) {
	r := NewDirectory("local", time.Second, "token")
	defer r.Close()
	if err := r.Update([]Member{{ID: "remote", Address: "127.0.0.1:8001"}}); err != nil {
		t.Fatal(err)
	}
	if r.state.Load().ring != nil {
		t.Fatal("directory built read ring")
	}
	if _, ok := r.PickPeer("key"); ok {
		t.Fatal("directory routed read")
	}
	if len(r.Members()) != 1 || len(r.state.Load().clients) != 1 {
		t.Fatal("lost invalidation membership")
	}
}
