package hostinfo

import (
	"testing"
	"time"
)

func TestParseSMI(t *testing.T) {
	gs := parseSMI("0, NVIDIA GeForce RTX 4090 Laptop GPU, 7, 313, 16376, 61, 4.75, [N/A]\n1, NVIDIA GB10, 95, [N/A], [N/A], 70, 40.1, 100\n")
	if len(gs) != 2 {
		t.Fatalf("%+v", gs)
	}
	if g := gs[0]; g.Util != 7 || g.MemUsed != 313<<20 || g.MemTotal != 16376<<20 || g.Temp != 61 || g.PowerLimit != -1 {
		t.Fatalf("%+v", g)
	}
	if g := gs[1]; g.MemUsed != -1 || g.MemTotal != -1 || g.PowerLimit != 100 { // unified memory: no VRAM figures
		t.Fatalf("%+v", g)
	}
}

func TestSampleThisMachine(t *testing.T) {
	s := New()
	s.Snapshot()
	time.Sleep(300 * time.Millisecond)
	s.sample()
	sn := s.Snapshot()
	if sn.Now.RAMTotal <= 0 || sn.Static.Threads == 0 {
		t.Fatalf("no RAM / CPU figures: %+v", sn)
	}
	if sn.Now.CPU < 0 || sn.Now.CPU > 100 {
		t.Fatalf("cpu %% out of range: %v", sn.Now.CPU)
	}
	t.Logf("%+v", sn.Static)
	t.Logf("cpu %.1f%% load %.2f ram %d/%d MiB temps %+v gpus %+v disks %+v io %.1f/%.1f MB/s notes %v",
		sn.Now.CPU, sn.Now.Load1, sn.Now.RAMUsed>>20, sn.Now.RAMTotal>>20, sn.Now.Temps, sn.Now.GPUs, sn.Now.Disks, sn.Now.DiskRead, sn.Now.DiskWrite, sn.Notes)
}
