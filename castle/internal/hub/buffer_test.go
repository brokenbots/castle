package hub

import (
	"fmt"
	"sync"
	"testing"

	pb "github.com/brokenbots/castle/shared/pb/overlord/v1" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

func TestBuffer_AppendAndDrain(t *testing.T) {
	b := NewBuffer(8)
	for i := 1; i <= 5; i++ {
		b.Append(&pb.Envelope{RunId: "r1", Seq: uint64(i)})
	}

	got := b.Since("r1", 0)
	if len(got) != 5 {
		t.Fatalf("len=%d want 5", len(got))
	}
	for i, env := range got {
		want := uint64(i + 1)
		if env.Seq != want {
			t.Fatalf("seq[%d]=%d want %d", i, env.Seq, want)
		}
	}
}

func TestBuffer_SinceHonored(t *testing.T) {
	b := NewBuffer(8)
	for i := 1; i <= 5; i++ {
		b.Append(&pb.Envelope{RunId: "r1", Seq: uint64(i)})
	}

	got := b.Since("r1", 3)
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
	if got[0].Seq != 4 || got[1].Seq != 5 {
		t.Fatalf("seqs=%d,%d want 4,5", got[0].Seq, got[1].Seq)
	}
}

func TestBuffer_CapacityEviction(t *testing.T) {
	b := NewBuffer(3)
	for i := 1; i <= 5; i++ {
		b.Append(&pb.Envelope{RunId: "r1", Seq: uint64(i)})
	}

	got := b.Since("r1", 0)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	if got[0].Seq != 3 || got[1].Seq != 4 || got[2].Seq != 5 {
		t.Fatalf("seqs=%d,%d,%d want 3,4,5", got[0].Seq, got[1].Seq, got[2].Seq)
	}
}

func TestBuffer_Forget(t *testing.T) {
	b := NewBuffer(8)
	b.Append(&pb.Envelope{RunId: "r1", Seq: 1})
	b.Forget("r1")

	got := b.Since("r1", 0)
	if len(got) != 0 {
		t.Fatalf("len=%d want 0", len(got))
	}
}

func TestBuffer_ConcurrentAppendAndSince(t *testing.T) {
	b := NewBuffer(4096)
	const total = 2000
	errCh := make(chan error, 1)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 1; i <= total; i++ {
			b.Append(&pb.Envelope{RunId: "r1", Seq: uint64(i)})
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < total; i++ {
			got := b.Since("r1", 0)
			for j := 1; j < len(got); j++ {
				if got[j].Seq <= got[j-1].Seq {
					select {
					case errCh <- fmt.Errorf("out of order: seq[%d]=%d <= seq[%d]=%d", j, got[j].Seq, j-1, got[j-1].Seq):
					default:
					}
					return
				}
			}
		}
	}()
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}

	seen := map[uint64]struct{}{}
	for _, env := range b.Since("r1", 0) {
		if _, ok := seen[env.Seq]; ok {
			t.Fatalf("duplicate seq %d", env.Seq)
		}
		seen[env.Seq] = struct{}{}
	}
}

func TestBuffer_EvictionCallback(t *testing.T) {
	b := NewBuffer(2)
	var mu sync.Mutex
	var evicted []uint64
	b.onEvict = func(_ string, oldestSeq uint64) {
		mu.Lock()
		evicted = append(evicted, oldestSeq)
		mu.Unlock()
	}

	for i := 1; i <= 4; i++ {
		b.Append(&pb.Envelope{RunId: "r1", Seq: uint64(i)})
	}

	mu.Lock()
	defer mu.Unlock()
	if len(evicted) != 2 {
		t.Fatalf("evictions=%d want 2", len(evicted))
	}
	if evicted[0] != 1 || evicted[1] != 2 {
		t.Fatalf("evicted seqs=%v want [1 2]", evicted)
	}
}

func TestHub_AppendBeforeFanOut(t *testing.T) {
	h := NewWithBuffer(16, nil)
	sub := h.Subscribe("r1")
	defer h.Unsubscribe(sub)

	env := &pb.Envelope{RunId: "r1", Seq: 1}
	h.Publish(env)

	got := h.Since("r1", 0)
	if len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("buffer missing published event: %v", summarizeSeqs(got))
	}

	select {
	case recv := <-sub.C:
		if recv.Seq != 1 {
			t.Fatalf("subscriber seq=%d want 1", recv.Seq)
		}
	default:
		t.Fatal("subscriber did not receive published event")
	}
}

func summarizeSeqs(envs []*pb.Envelope) string {
	if len(envs) == 0 {
		return "[]"
	}
	out := "["
	for i, env := range envs {
		if i > 0 {
			out += " "
		}
		out += fmt.Sprintf("%d", env.Seq)
	}
	out += "]"
	return out
}
