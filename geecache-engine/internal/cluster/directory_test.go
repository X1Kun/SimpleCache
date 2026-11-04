package cluster

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestDirectorySnapshotsOwnMembersAndTransportAddresses(t *testing.T) {
	d := NewDirectory("self", time.Second, "token")
	defer d.Close()
	input := []Member{{ID: "b", Address: "127.0.0.1:8002"}, {ID: "a", Address: "127.0.0.1:8003"}, {ID: "a", Address: "127.0.0.1:8001"}}
	if err := d.Update(input); err != nil {
		t.Fatal(err)
	}
	old := d.state.Load()
	input[0].ID = "mutated"
	got := d.Members()
	if len(got) != 2 || got[0].Address != "127.0.0.1:8001" {
		t.Fatal(got)
	}
	got[0].ID = "mutated"
	if d.Members()[0].ID != "a" {
		t.Fatal("caller mutated directory")
	}
	if err := d.Update([]Member{{ID: "a", Address: "127.0.0.1:9001"}}); err != nil {
		t.Fatal(err)
	}
	if old.clients["a"].address != "127.0.0.1:8001" || d.state.Load().clients["a"].address != "127.0.0.1:9001" {
		t.Fatal("update mutated a captured delivery snapshot")
	}
	before := d.state.Load()
	if d.Update([]Member{{ID: "bad", Address: "bad"}}) == nil || d.state.Load() != before {
		t.Fatal("invalid update corrupted snapshot")
	}
}
func TestDirectoryPublicationWithConcurrentReaders(t *testing.T) {
	d := NewDirectory("self", time.Second, "token")
	defer d.Close()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Go(func() {
			for j := 0; j < 200; j++ {
				members := d.Members()
				for _, m := range members {
					if m.ID != "member" {
						t.Error(m)
					}
				}
			}
		})
	}
	for i := 0; i < 200; i++ {
		if err := d.Update([]Member{{ID: "member", Address: fmt.Sprintf("127.0.0.1:%d", 8001+i)}}); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}
