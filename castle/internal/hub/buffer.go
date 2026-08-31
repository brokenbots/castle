package hub

import (
	"sync"

	overseer "github.com/brokenbots/castle/shared/sdk/overseer"
)

// Buffer keeps a bounded in-memory ring of recent envelopes per run.
type Buffer struct {
	mu        sync.RWMutex
	capPerRun int
	byRun     map[string]*runRing

	// onEvict is used by Hub for observability and is intentionally optional.
	onEvict func(runID string, oldestSeq uint64)
}

type runRing struct {
	items  []*overseer.Envelope
	start  int
	size   int
	newest uint64
}

func NewBuffer(capPerRun int) *Buffer {
	if capPerRun <= 0 {
		capPerRun = DefaultEventBufferCapacity
	}
	return &Buffer{capPerRun: capPerRun, byRun: make(map[string]*runRing)}
}

func (b *Buffer) Append(env *overseer.Envelope) {
	if b == nil || env == nil || env.RunId == "" {
		return
	}

	b.mu.Lock()
	r, ok := b.byRun[env.RunId]
	if !ok {
		r = &runRing{items: make([]*overseer.Envelope, b.capPerRun)}
		b.byRun[env.RunId] = r
	}
	evicted, oldestSeq := r.append(env)
	onEvict := b.onEvict
	runID := env.RunId
	b.mu.Unlock()

	if evicted && onEvict != nil {
		onEvict(runID, oldestSeq)
	}
}

func (b *Buffer) Since(runID string, seq uint64) []*overseer.Envelope {
	if b == nil || runID == "" {
		return nil
	}

	b.mu.RLock()
	r := b.byRun[runID]
	if r == nil || r.size == 0 || r.newest <= seq {
		b.mu.RUnlock()
		return nil
	}
	out := r.since(seq)
	b.mu.RUnlock()
	return out
}

func (b *Buffer) Forget(runID string) {
	if b == nil || runID == "" {
		return
	}
	b.mu.Lock()
	delete(b.byRun, runID)
	b.mu.Unlock()
}

func (r *runRing) append(env *overseer.Envelope) (bool, uint64) {
	if r.size < len(r.items) {
		idx := (r.start + r.size) % len(r.items)
		r.items[idx] = env
		r.size++
		r.newest = env.Seq
		return false, 0
	}

	evicted := r.items[r.start]
	evictedSeq := uint64(0)
	if evicted != nil {
		evictedSeq = evicted.Seq
	}
	r.items[r.start] = env
	r.start = (r.start + 1) % len(r.items)
	r.newest = env.Seq
	return true, evictedSeq
}

func (r *runRing) since(seq uint64) []*overseer.Envelope {
	if r == nil || r.size == 0 {
		return nil
	}
	out := make([]*overseer.Envelope, 0, r.size)
	for i := 0; i < r.size; i++ {
		env := r.items[(r.start+i)%len(r.items)]
		if env == nil || env.Seq <= seq {
			continue
		}
		out = append(out, env)
	}
	return out
}
