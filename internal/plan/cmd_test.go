package plan

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectShards(t *testing.T) {
	// 1. Non-sharded file
	if si := DetectShards("/path/to/llama3.gguf"); si != nil {
		t.Fatalf("expected nil for non-sharded model, got %+v", si)
	}

	// 2. Sharded files in a directory
	tmpDir := t.TempDir()
	s1 := filepath.Join(tmpDir, "DeepSeek-V4.1-Flash-00001-of-00003.gguf")
	s2 := filepath.Join(tmpDir, "DeepSeek-V4.1-Flash-00002-of-00003.gguf")
	s3 := filepath.Join(tmpDir, "DeepSeek-V4.1-Flash-00003-of-00003.gguf")

	_ = os.WriteFile(s1, make([]byte, 1000), 0644)
	_ = os.WriteFile(s2, make([]byte, 2000), 0644)
	_ = os.WriteFile(s3, make([]byte, 3000), 0644)

	// Test passing shard 2: must resolve first shard, total 3, totalBytes 6000
	si := DetectShards(s2)
	if si == nil {
		t.Fatal("expected shard detection to succeed")
	}
	if si.BaseName != "DeepSeek-V4.1-Flash" {
		t.Fatalf("expected base name DeepSeek-V4.1-Flash, got %s", si.BaseName)
	}
	if si.Total != 3 || si.Index != 2 {
		t.Fatalf("expected total 3 and index 2, got total %d index %d", si.Total, si.Index)
	}
	if si.FirstShard != s1 {
		t.Fatalf("expected first shard %s, got %s", s1, si.FirstShard)
	}
	if si.TotalBytes != 6000 {
		t.Fatalf("expected total bytes 6000, got %d", si.TotalBytes)
	}
	if len(si.Missing) != 0 {
		t.Fatalf("expected 0 missing, got %v", si.Missing)
	}

	// 3. Sharded with missing shard 2
	os.Remove(s2)
	siMissing := DetectShards(s1)
	if siMissing == nil {
		t.Fatal("expected detection with missing parts")
	}
	if len(siMissing.Missing) != 1 || siMissing.Missing[0] != 2 {
		t.Fatalf("expected missing shard [2], got %v", siMissing.Missing)
	}
	if siMissing.TotalBytes != 4000 {
		t.Fatalf("expected total bytes 4000, got %d", siMissing.TotalBytes)
	}
}
