package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
)

// ShardsSchema is the version of fleet-shards.json.
const ShardsSchema = 1

// Shards is fleet-shards.json: for one single-file GGUF, which bytes belong to which block, so that a worker that
// holds blocks a..b needs only those byte ranges (plus nothing of the others) and can check them by hash.
type Shards struct {
	Schema    int         `json:"schema"`
	Model     string      `json:"model"`
	Arch      string      `json:"arch"`
	FileBytes int64       `json:"file_bytes"`
	Layers    int         `json:"layers"`
	Blocks    []ShardPart `json:"blocks"`  // blk.0 ... blk.N-1
	Globals   []ShardPart `json:"globals"` // token embedding, output projection, final norm: tensors outside the blocks
}

// ShardPart is the tensors of one block (or the global ones). Their bytes are not always one run in the file, so the
// part lists merged byte ranges; Sha256 is over those ranges concatenated in file order ("" with --no-hash).
type ShardPart struct {
	Block   int         `json:"block"` // -1 for the globals
	Bytes   int64       `json:"bytes"`
	Tensors []string    `json:"tensors"`
	Ranges  []ByteRange `json:"ranges"`
	Sha256  string      `json:"sha256,omitempty"`
}

// ByteRange is [Offset, Offset+Length) of the file.
type ByteRange struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

// BuildShards reads the header of path and, when hash is set, hashes every part (a full read of the file).
func BuildShards(path string, hash bool) (*Shards, error) {
	m, err := ReadGGUF(path)
	if err != nil {
		return nil, err
	}
	if m.Shards != nil { // tensors of several files do not share one offset space
		return nil, fmt.Errorf("%s is one file of a multi-file GGUF (multi-file models are not described yet)", path)
	}
	sh := &Shards{Schema: ShardsSchema, Model: m.Name, Arch: m.Arch, FileBytes: m.FileBytes, Layers: m.Layers}
	byBlock := map[int][]Tensor{}
	var globals []Tensor
	for _, t := range m.Tensors {
		if b, ok := blockOf(t.Name); ok {
			byBlock[b] = append(byBlock[b], t)
		} else {
			globals = append(globals, t)
		}
	}
	var f *os.File
	if hash {
		if f, err = os.Open(path); err != nil {
			return nil, err
		}
		defer f.Close()
	}
	mk := func(block int, ts []Tensor) (ShardPart, error) {
		sort.Slice(ts, func(i, j int) bool { return ts[i].Offset < ts[j].Offset })
		p := ShardPart{Block: block}
		for _, t := range ts {
			p.Tensors = append(p.Tensors, t.Name)
			p.Bytes += t.Size
			if n := len(p.Ranges); n > 0 && p.Ranges[n-1].Offset+p.Ranges[n-1].Length == t.Offset {
				p.Ranges[n-1].Length += t.Size
			} else {
				p.Ranges = append(p.Ranges, ByteRange{t.Offset, t.Size})
			}
		}
		if hash {
			h := sha256.New()
			for _, r := range p.Ranges {
				if _, err := io.Copy(h, io.NewSectionReader(f, r.Offset, r.Length)); err != nil {
					return p, err
				}
			}
			p.Sha256 = hex.EncodeToString(h.Sum(nil))
		}
		return p, nil
	}
	for b := 0; b < m.Layers+m.MTPLayers; b++ {
		if len(byBlock[b]) == 0 {
			continue
		}
		p, err := mk(b, byBlock[b])
		if err != nil {
			return nil, err
		}
		sh.Blocks = append(sh.Blocks, p)
	}
	g, err := mk(-1, globals)
	if err != nil {
		return nil, err
	}
	sh.Globals = []ShardPart{g}
	return sh, nil
}

// Need is what a worker that holds blocks first..last must have: the byte ranges (merged) and their size.
// withGlobals adds the token embedding/output/norm tensors (the node that runs the output projection needs them).
func (s *Shards) Need(first, last int, withGlobals bool) (ranges []ByteRange, bytes int64) {
	var rs []ByteRange
	for _, b := range s.Blocks {
		if b.Block >= first && b.Block <= last {
			rs = append(rs, b.Ranges...)
		}
	}
	if withGlobals {
		for _, g := range s.Globals {
			rs = append(rs, g.Ranges...)
		}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Offset < rs[j].Offset })
	for _, r := range rs {
		if n := len(ranges); n > 0 && ranges[n-1].Offset+ranges[n-1].Length == r.Offset {
			ranges[n-1].Length += r.Length
		} else {
			ranges = append(ranges, r)
		}
		bytes += r.Length
	}
	return
}

// Verify hashes the parts of the blocks first..last in path and compares them with the manifest.
func (s *Shards) Verify(path string, first, last int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, b := range s.Blocks {
		if b.Block < first || b.Block > last {
			continue
		}
		if b.Sha256 == "" {
			return fmt.Errorf("block %d has no hash in the manifest", b.Block)
		}
		h := sha256.New()
		for _, r := range b.Ranges {
			if _, err := io.Copy(h, io.NewSectionReader(f, r.Offset, r.Length)); err != nil {
				return err
			}
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != b.Sha256 {
			return fmt.Errorf("block %d: hash %s, manifest says %s", b.Block, got[:12], b.Sha256[:12])
		}
	}
	return nil
}
