package config

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLockSerializesWriters(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	un, err := Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	got := false
	done := make(chan struct{})
	go func() {
		u2, err := Lock(p)
		if err != nil {
			t.Error(err)
			close(done)
			return
		}
		mu.Lock()
		got = true
		mu.Unlock()
		u2()
		close(done)
	}()
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	if got {
		t.Fatal("a second Lock must wait while the first is held")
	}
	mu.Unlock()
	un()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the second Lock never got it after release")
	}
}
