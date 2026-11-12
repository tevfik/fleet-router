package rtt

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func fake(ds ...time.Duration) (Probe, *int) {
	n := 0
	return func(ctx context.Context, addr string) (time.Duration, error) {
		d := ds[n%len(ds)]
		n++
		if d < 0 {
			return 0, errors.New("refused")
		}
		return d, nil
	}, &n
}

func TestMeasureMedian(t *testing.T) {
	p, _ := fake(100*time.Millisecond, 46*time.Millisecond, 45*time.Millisecond, 47*time.Millisecond, 48*time.Millisecond)
	s, err := Measure(context.Background(), p, "w:22", 5)
	if err != nil || s.MS != 47 || s.N != 5 || s.MaxMS != 100 {
		t.Fatalf("%+v %v", s, err)
	}
	p, _ = fake(-1, 10*time.Millisecond, 20*time.Millisecond, -1)
	s, err = Measure(context.Background(), p, "w:22", 4)
	if err != nil || s.MS != 15 || s.N != 2 {
		t.Fatalf("even count / drops: %+v %v", s, err)
	}
	p, _ = fake(-1)
	if _, err = Measure(context.Background(), p, "w:22", 3); err == nil {
		t.Fatal("all probes failed must be an error")
	}
}

func TestCollectCacheAndSkip(t *testing.T) {
	p, n := fake(45 * time.Millisecond)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	c := &Collector{From: "p1", Probe: p, Cache: &Cache{Path: filepath.Join(t.TempDir(), "rtt.json")}, Now: func() time.Time { return now }, Samples: 3,
		Skip: map[string]bool{"vec>p1": true}}
	tg := []Target{{"dino", "dino:22"}, {"vec", "vec:22"}}
	m, d := c.Collect(context.Background(), tg)
	if m["p1>dino"] != 45 || len(m) != 1 || d["p1>vec"].Skipped == "" || *n != 3 {
		t.Fatalf("%v %v probes=%d", m, d, *n)
	}
	// fresh: served from the cache, no probes
	now = now.Add(5 * time.Minute)
	m, d = c.Collect(context.Background(), tg)
	if m["p1>dino"] != 45 || !d["p1>dino"].Cached || *n != 3 {
		t.Fatalf("cache not used: %v probes=%d", m, *n)
	}
	// stale: measured again
	now = now.Add(time.Hour)
	c.Collect(context.Background(), tg)
	if *n != 6 {
		t.Fatalf("stale entry not refreshed: probes=%d", *n)
	}
	// refresh flag
	c.Refresh = true
	c.Collect(context.Background(), tg)
	if *n != 9 {
		t.Fatalf("--refresh ignored: probes=%d", *n)
	}
}

func TestCollectUnreachableLeftOut(t *testing.T) {
	p, _ := fake(-1)
	c := &Collector{From: "a", Probe: p, Samples: 2}
	m, d := c.Collect(context.Background(), []Target{{"b", "b:22"}})
	if len(m) != 0 || d["a>b"].Skipped == "" {
		t.Fatalf("%v %v", m, d)
	}
}
