package server

import (
	"container/heap"
	"context"
	"sync"
)

// limiter is a priority counting semaphore. cap=0 → unlimited. Among waiters the higher priority goes first, then the oldest.
type limiter struct {
	mu   sync.Mutex
	cap  int
	used int
	seq  int
	q    waitQ
}

type waiter struct {
	prio, seq, idx int
	ch             chan struct{}
	granted        bool
}

type waitQ []*waiter

func (q waitQ) Len() int { return len(q) }
func (q waitQ) Less(i, j int) bool {
	if q[i].prio != q[j].prio {
		return q[i].prio > q[j].prio
	}
	return q[i].seq < q[j].seq
}
func (q waitQ) Swap(i, j int) { q[i], q[j] = q[j], q[i]; q[i].idx, q[j].idx = i, j }
func (q *waitQ) Push(x any)   { w := x.(*waiter); w.idx = len(*q); *q = append(*q, w) }
func (q *waitQ) Pop() any     { o := *q; n := len(o); w := o[n-1]; *q = o[:n-1]; return w }

func (l *limiter) setCap(n int) {
	l.mu.Lock()
	l.cap = n
	l.grant()
	l.mu.Unlock()
}

func (l *limiter) stats() (used, waiting, cap int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.used, len(l.q), l.cap
}

// grant wakes as many waiters (in priority order) as there are free slots. Called with mu held.
func (l *limiter) grant() {
	for len(l.q) > 0 && (l.cap == 0 || l.used < l.cap) {
		w := heap.Pop(&l.q).(*waiter)
		w.granted = true
		l.used++
		close(w.ch)
	}
}

// try attempts to take a slot without waiting (never jumps ahead of queued waiters).
func (l *limiter) try() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if (l.cap == 0 || l.used < l.cap) && len(l.q) == 0 {
		l.used++
		return true
	}
	return false
}

func (l *limiter) release() {
	l.mu.Lock()
	if l.used > 0 {
		l.used--
	}
	l.grant()
	l.mu.Unlock()
}

// acquire waits until a slot is granted (or ctx ends). On success the caller must call release.
func (l *limiter) acquire(ctx context.Context, prio int) error {
	l.mu.Lock()
	if (l.cap == 0 || l.used < l.cap) && len(l.q) == 0 {
		l.used++
		l.mu.Unlock()
		return nil
	}
	l.seq++
	w := &waiter{prio: prio, seq: l.seq, ch: make(chan struct{})}
	heap.Push(&l.q, w)
	l.mu.Unlock()
	select {
	case <-w.ch:
		return nil
	case <-ctx.Done():
		l.mu.Lock()
		if w.granted { // granted just now → give it back
			l.used--
			l.grant()
		} else {
			heap.Remove(&l.q, w.idx)
		}
		l.mu.Unlock()
		return ctx.Err()
	}
}
