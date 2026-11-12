// Package plan works out where a model should run: on one machine, or split over several. It reads what the
// model is made of (the GGUF header), takes what is known about the machines, and prices every candidate in
// milliseconds per token with the cost model measured on llama-ring (docs/distributed.md). It changes nothing.
package plan

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Model is what a GGUF file says about the model, in the terms the planner needs.
type Model struct {
	Name   string `json:"name"`
	Arch   string `json:"arch"`
	Layers int    `json:"layers"`
	Embd   int    `json:"embd"`
	CtxMax int    `json:"ctx_max,omitempty"`

	Experts     int `json:"experts,omitempty"`      // 0: dense
	ExpertsUsed int `json:"experts_used,omitempty"` // experts one token uses
	// ExpertBytes: the MoE expert tensors (blk.N.*_exps) of each block, what the CPU-expert tier can move to host RAM.
	ExpertBytes []int64 `json:"expert_bytes,omitempty"`

	FileBytes   int64   `json:"file_bytes"`
	LayerBytes  []int64 `json:"layer_bytes"`  // weights of each block
	LayerRead   []int64 `json:"layer_read"`   // bytes one token reads from each block (a mixture of experts reads few)
	OtherBytes  int64   `json:"other_bytes"`  // embedding, output, norms: everything outside the blocks
	OutputBytes int64   `json:"output_bytes"` // the output projection, read for every token
	EmbdBytes   int64   `json:"embd_bytes"`   // the token embedding table (it stays on the master)

	KVPerTokenLayer int64 `json:"kv_per_token_layer"` // K and V of one token in one block (f16)
	HiddenBytes     int64 `json:"hidden_bytes"`       // the state that crosses a machine boundary per token (f32)
	Vocab           int   `json:"vocab,omitempty"`    // tokens in the vocabulary: the output projection's logits (Vocab × 4 bytes) are what comes back per token when a worker holds it

	HasMTP    bool `json:"has_mtp,omitempty"`    // true if model has embedded NextN / MTP layers
	MTPLayers int  `json:"mtp_layers,omitempty"` // number of MTP draft heads

	// Recurrent: some blocks keep a recurrent state (state-space, short-convolution or linear-attention layers)
	// instead of, or besides, a per-token KV cache. Read from the GGUF schema (<arch>.ssm.*, <arch>.shortconv.*,
	// <arch>.wkv.*, <arch>.full_attention_interval keys, or blk.N.ssm_* / shortconv / time_mix tensors), never from a name.
	Recurrent bool `json:"recurrent,omitempty"`
	// KVBlocks: how many blocks keep a per-token KV cache (0: all of them; a hybrid model has recurrent blocks
	// that keep a fixed-size state instead). Derived from the GGUF header and tensor table.
	KVBlocks int `json:"kv_blocks,omitempty"`

	Shards *ShardInfo `json:"shards,omitempty"` // multi-file shard details if sharded

	Runtime string `json:"runtime,omitempty"` // "" or "ring": llama-ring over a GGUF; "stage-pipe": a stage chain over a compiled model directory (fleet-model.json)

	Tensors []Tensor `json:"-"` // every tensor with its place in the file (read from the header, not sent around)
}

// Tensor is where one tensor's bytes are in the file.
type Tensor struct {
	Name   string
	Offset int64 // from the start of the file
	Size   int64
	Shard  int // index of the file of a multi-file GGUF this tensor is in (0: the first or only file)
}

// Weights is every weight in the file, in bytes (the part of the file that is tensors).
func (m *Model) Weights() int64 {
	t := m.OtherBytes
	for _, b := range m.LayerBytes {
		t += b
	}
	return t
}

// ExpertWeights is the sum of MoE expert weights across all blocks, in bytes.
func (m *Model) ExpertWeights() int64 {
	if m == nil {
		return 0
	}
	var t int64
	for _, b := range m.ExpertBytes {
		t += b
	}
	return t
}

// DenseWeights is the non-expert weights in the file (embedding, output, norms, attention, shared experts), in bytes.
func (m *Model) DenseWeights() int64 {
	if m == nil {
		return 0
	}
	return max(m.Weights()-m.ExpertWeights(), 0)
}

// Speculation returns the optimal speculative decoding strategy for this model:
// "draft-mtp" if native MTP draft heads exist, otherwise "ngram-mod" (context lookahead).
func (m *Model) Speculation() (specType string, draftTokens int, desc string) {
	if m != nil && m.HasMTP {
		n := 3
		if m.MTPLayers > 0 {
			n = m.MTPLayers
		}
		return "draft-mtp", n, fmt.Sprintf("MTP native (%d draft head(s))", n)
	}
	return "ngram-mod", 3, "n-gram prompt lookahead"
}

// ShardInfo holds multi-file shard metadata.
type ShardInfo struct {
	BaseName   string   `json:"base_name"`
	Index      int      `json:"index"`
	Total      int      `json:"total"`
	FirstShard string   `json:"first_shard"`
	ShardPaths []string `json:"shard_paths"`
	TotalBytes int64    `json:"total_bytes"`
	Missing    []int    `json:"missing,omitempty"`
	// Extra: other .gguf files next to the shards that share the model's name but are not shards of it (an Engram
	// table, a separate output head, ...). Their bytes are NOT in the weights; a model that needs one will not run
	// from the shards alone, so the planner warns.
	Extra      []string `json:"extra,omitempty"`
	ExtraBytes int64    `json:"extra_bytes,omitempty"`
}

var shardRegex = regexp.MustCompile(`^(.+?)[-_.]0*([1-9][0-9]*)-of-0*([1-9][0-9]*)\.gguf$`)

// DetectShards inspects a file path to see if it is part of a sharded GGUF set.
func DetectShards(path string) *ShardInfo {
	base := filepath.Base(path)
	matches := shardRegex.FindStringSubmatch(base)
	if len(matches) < 4 {
		return nil
	}
	prefix := matches[1]
	idx, _ := strconv.Atoi(matches[2])
	total, _ := strconv.Atoi(matches[3])
	if total <= 1 || idx <= 0 || idx > total {
		return nil
	}

	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	si := &ShardInfo{
		BaseName:   prefix,
		Index:      idx,
		Total:      total,
		ShardPaths: make([]string, 0, total),
	}

	foundMap := map[int]string{}
	var totalBytes int64

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		subm := shardRegex.FindStringSubmatch(e.Name())
		if len(subm) != 4 && strings.HasSuffix(e.Name(), ".gguf") && strings.HasPrefix(e.Name(), prefix) &&
			!strings.Contains(strings.ToLower(e.Name()), "mmproj") {
			si.Extra = append(si.Extra, filepath.Join(dir, e.Name()))
			if fi, err := e.Info(); err == nil {
				si.ExtraBytes += fi.Size()
			}
			continue
		}
		if len(subm) == 4 && subm[1] == prefix {
			t, _ := strconv.Atoi(subm[3])
			if t == total {
				curIdx, _ := strconv.Atoi(subm[2])
				fullP := filepath.Join(dir, e.Name())
				foundMap[curIdx] = fullP
				if fi, err := e.Info(); err == nil {
					totalBytes += fi.Size()
				}
			}
		}
	}

	for i := 1; i <= total; i++ {
		if p, ok := foundMap[i]; ok {
			si.ShardPaths = append(si.ShardPaths, p)
			if i == 1 {
				si.FirstShard = p
			}
		} else {
			si.Missing = append(si.Missing, i)
		}
	}
	if si.FirstShard == "" && len(si.ShardPaths) > 0 {
		si.FirstShard = si.ShardPaths[0]
	}
	si.TotalBytes = totalBytes
	return si
}

// ReadGGUF reads the header of a GGUF file (automatically detecting multi-file shards).
func ReadGGUF(path string) (*Model, error) {
	si := DetectShards(path)
	targetPath := path
	if si != nil && si.FirstShard != "" {
		targetPath = si.FirstShard
	}
	f, err := os.Open(targetPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	m, err := ParseGGUF(f, st.Size())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", targetPath, err)
	}
	if si != nil {
		if err := m.addShards(si, targetPath); err != nil {
			return nil, err
		}
		if m.Name == "" || strings.Contains(m.Name, "-of-") {
			m.Name = si.BaseName
		}
	} else if m.Name == "" {
		m.Name = strings.TrimSuffix(baseName(path), ".gguf")
	}
	return m, nil
}

// ShardNames lists the paths of every shard of the multi-file GGUF that path is one file of (same directory, same
// zero-padding as path's own name), or nil when path is a single file. The files need not exist.
func ShardNames(path string) []string {
	m := shardRegex.FindStringSubmatch(filepath.Base(path))
	if len(m) < 4 {
		return nil
	}
	total, _ := strconv.Atoi(m[3])
	if total <= 1 {
		return nil
	}
	// the base is prefix + sep + number + "-of-" + total + ".gguf": the widths are what the name itself has
	rest := strings.TrimPrefix(filepath.Base(path), m[1])
	sep := rest[:1]
	of := strings.Index(rest, "-of-")
	numW := of - len(sep)
	totW := len(strings.TrimSuffix(rest[of+4:], ".gguf"))
	dir := filepath.Dir(path)
	out := make([]string, 0, total)
	for i := 1; i <= total; i++ {
		out = append(out, filepath.Join(dir, fmt.Sprintf("%s%s%0*d-of-%0*d.gguf", m[1], sep, numW, i, totW, total)))
	}
	return out
}

// AddShardFile adds the tensors of another file of a multi-file GGUF (r positioned at its start, size its length;
// shard is its 0-based index among the model's files) to m: every file lists only its own tensors, the metadata
// is in the first.
func (m *Model) AddShardFile(r io.Reader, size int64, shard int) error {
	h, err := parseHeader(r)
	if err != nil {
		return err
	}
	m.addTensors(h.ts, h.dataStart, size, shard)
	return nil
}

// addShards reads the tensor tables of the other files of a multi-file GGUF (each file lists only its own tensors) so
// that the weights, the per-block bytes and the file size are those of the whole model, not of the first file alone.
func (m *Model) addShards(si *ShardInfo, first string) error {
	m.Shards = si
	if len(si.Missing) > 0 {
		for _, i := range si.Missing {
			if i == 1 {
				return fmt.Errorf("%s: shard 1 of %d is missing (it holds the model's metadata)", si.BaseName, si.Total)
			}
		}
	}
	m.FileBytes = si.TotalBytes
	for i, p := range si.ShardPaths {
		if p == first {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		h, err := parseHeader(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		// ShardPaths lists the files that exist, in shard order; the index is the file's position among them.
		m.addTensors(h.ts, h.dataStart, st.Size(), i)
	}
	return nil
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

type reader struct {
	r *bufio.Reader
	n int64
}

func (c *reader) full(b []byte) error {
	k, err := io.ReadFull(c.r, b)
	c.n += int64(k)
	return err
}
func (c *reader) u32() (uint32, error) {
	var b [4]byte
	err := c.full(b[:])
	return binary.LittleEndian.Uint32(b[:]), err
}
func (c *reader) u64() (uint64, error) {
	var b [8]byte
	err := c.full(b[:])
	return binary.LittleEndian.Uint64(b[:]), err
}
func (c *reader) skip(n int64) error {
	k, err := c.r.Discard(int(n)) // n is bounded by the caller
	c.n += int64(k)
	return err
}

const maxString = 1 << 20

func (c *reader) str(keep bool) (string, error) {
	n, err := c.u64()
	if err != nil {
		return "", err
	}
	if n > 1<<30 {
		return "", fmt.Errorf("a string of %d bytes: not a GGUF file", n)
	}
	if !keep || n > maxString {
		for left := int64(n); left > 0; {
			k := min(left, 1<<30)
			if err := c.skip(k); err != nil {
				return "", err
			}
			left -= k
		}
		return "", nil
	}
	b := make([]byte, n)
	return string(b), c.full(b)
}

// size of the fixed-width value types of GGUF
var scalarSize = map[uint32]int64{0: 1, 1: 1, 2: 2, 3: 2, 4: 4, 5: 4, 6: 4, 7: 1, 10: 8, 11: 8, 12: 8}

func isInt(t uint32) bool { return t <= 5 || t == 10 || t == 11 }

// value reads one metadata value. Integers come back in n; a string in s; anything else is skipped.
// An array of integers (per-layer head counts) comes back as its largest element.
func (c *reader) value(t uint32, keep bool) (n uint64, ok bool, s string, err error) {
	switch {
	case isInt(t):
		var b [8]byte
		sz := scalarSize[t]
		if err = c.full(b[:sz]); err != nil {
			return
		}
		n = binary.LittleEndian.Uint64(b[:])
		switch t { // sign-extend nothing: counts are not negative; a negative one reads as huge and is ignored later
		case 0, 1:
			n &= 0xff
		case 2, 3:
			n &= 0xffff
		case 4, 5:
			n &= 0xffffffff
		}
		return n, true, "", nil
	case t == 8:
		s, err = c.str(keep)
		return
	case t == 9:
		var et uint32
		var cnt uint64
		if et, err = c.u32(); err != nil {
			return
		}
		if cnt, err = c.u64(); err != nil {
			return
		}
		switch {
		case isInt(et) && cnt <= 4096:
			var mx uint64
			for i := uint64(0); i < cnt; i++ {
				v, _, _, e := c.value(et, false)
				if e != nil {
					return 0, false, "", e
				}
				mx = max(mx, v)
			}
			return mx, true, "", nil
		case et == 8:
			for i := uint64(0); i < cnt; i++ {
				if _, e := c.str(false); e != nil {
					return 0, false, "", e
				}
			}
		case scalarSize[et] > 0:
			for left := int64(cnt) * scalarSize[et]; left > 0; {
				k := min(left, 1<<30)
				if err = c.skip(k); err != nil {
					return
				}
				left -= k
			}
		default:
			return 0, false, "", fmt.Errorf("an array of type %d is not supported", et)
		}
		return 0, false, "", nil
	case scalarSize[t] > 0:
		return 0, false, "", c.skip(scalarSize[t])
	}
	return 0, false, "", fmt.Errorf("unknown metadata type %d", t)
}

type tensor struct {
	name string
	off  uint64
}

// ggufHeader is the parsed header of one GGUF file.
type ggufHeader struct {
	ints                map[string]uint64
	strs                map[string]string
	ts                  []tensor
	dataStart           int64
	vocabOut, vocabEmbd int
}

// parseHeader reads the metadata and the tensor table of one GGUF file.
func parseHeader(r io.Reader) (*ggufHeader, error) {
	c := &reader{r: bufio.NewReaderSize(r, 1<<20)}
	var magic [4]byte
	if err := c.full(magic[:]); err != nil || string(magic[:]) != "GGUF" {
		return nil, fmt.Errorf("not a GGUF file")
	}
	ver, err := c.u32()
	if err != nil || ver < 2 || ver > 3 {
		return nil, fmt.Errorf("GGUF version %d is not supported", ver)
	}
	nt, err := c.u64()
	if err != nil {
		return nil, err
	}
	nkv, err := c.u64()
	if err != nil {
		return nil, err
	}
	if nt > 1<<20 || nkv > 1<<20 {
		return nil, fmt.Errorf("a header of %d tensors and %d values: not a GGUF file", nt, nkv)
	}
	ints, strs := map[string]uint64{}, map[string]string{}
	for i := uint64(0); i < nkv; i++ {
		k, err := c.str(true)
		if err != nil {
			return nil, err
		}
		t, err := c.u32()
		if err != nil {
			return nil, err
		}
		n, isn, s, err := c.value(t, k == "general.architecture" || k == "general.name")
		if err != nil {
			return nil, fmt.Errorf("value %q: %w", k, err)
		}
		if isn {
			ints[k] = n
		} else if s != "" {
			strs[k] = s
		}
	}
	var vocabOut, vocabEmbd int
	ts := make([]tensor, 0, nt)
	for i := uint64(0); i < nt; i++ {
		name, err := c.str(true)
		if err != nil {
			return nil, err
		}
		nd, err := c.u32()
		if err != nil || nd > 8 {
			return nil, fmt.Errorf("tensor %q: %d dimensions", name, nd)
		}
		var dims []uint64
		for d := uint32(0); d < nd; d++ {
			v, err := c.u64()
			if err != nil {
				return nil, err
			}
			dims = append(dims, v)
		}
		if len(dims) >= 2 && dims[1] > 0 && dims[1] < 1<<24 {
			switch name { // [embedding, vocabulary]; the output tensor wins over the (maybe tied) embedding table
			case "output.weight":
				vocabOut = int(dims[1])
			case "token_embd.weight":
				vocabEmbd = int(dims[1])
			}
		}
		if _, err := c.u32(); err != nil { // type
			return nil, err
		}
		off, err := c.u64()
		if err != nil {
			return nil, err
		}
		ts = append(ts, tensor{name, off})
	}
	align := int64(ints["general.alignment"])
	if align <= 0 {
		align = 32
	}
	dataStart := (c.n + align - 1) / align * align
	return &ggufHeader{ints: ints, strs: strs, ts: ts, dataStart: dataStart, vocabOut: vocabOut, vocabEmbd: vocabEmbd}, nil
}

// ParseGGUF reads a GGUF header from r; size is the whole file's size (the last tensor's size follows from it).
func ParseGGUF(r io.Reader, size int64) (*Model, error) {
	h, err := parseHeader(r)
	if err != nil {
		return nil, err
	}
	ints, strs, ts, dataStart, vocabOut, vocabEmbd := h.ints, h.strs, h.ts, h.dataStart, h.vocabOut, h.vocabEmbd
	arch := strs["general.architecture"]
	if arch == "" {
		return nil, fmt.Errorf("no general.architecture")
	}
	geti := func(k string) int { return int(ints[arch+"."+k]) }
	m := &Model{Name: strs["general.name"], Arch: arch, FileBytes: size, Layers: geti("block_count"), Embd: geti("embedding_length"),
		CtxMax: geti("context_length"), Experts: geti("expert_count"), ExpertsUsed: geti("expert_used_count")}
	if m.Layers <= 0 || m.Layers > 4096 || m.Embd <= 0 {
		return nil, fmt.Errorf("%s: block_count %d, embedding_length %d (no %s.block_count / %s.embedding_length: an architecture the planner does not know)", arch, m.Layers, m.Embd, arch, arch)
	}
	heads, kvh, hd := geti("attention.head_count"), geti("attention.head_count_kv"), geti("attention.key_length")
	if kvh == 0 {
		kvh = heads
	}
	if hd == 0 && heads > 0 {
		hd = m.Embd / heads
	}
	m.KVPerTokenLayer = int64(2 * kvh * hd * 2) // K and V, f16
	m.HiddenBytes = int64(m.Embd) * 4
	m.Vocab = vocabOut
	if m.Vocab == 0 {
		m.Vocab = vocabEmbd
	}
	m.LayerBytes, m.LayerRead = make([]int64, m.Layers), make([]int64, m.Layers)
	m.Recurrent = RecurrentKeys(arch, ints)
	if iv := geti("full_attention_interval"); iv > 1 && m.Layers >= iv {
		m.KVBlocks = m.Layers / iv
	}

	m.addTensors(ts, dataStart, size, 0)
	return m, nil
}

// addTensors adds the tensors of one file of the model (shard is its index among the files, 0 for the only or first
// one) to the per-block and global byte counts. The size of a tensor is the gap to the next one in the file, the last
// one runs to the end of the file (size).
func (m *Model) addTensors(ts []tensor, dataStart, size int64, shard int) {
	sort.Slice(ts, func(i, j int) bool { return ts[i].off < ts[j].off })
	moe := m.Experts > 0 && m.ExpertsUsed > 0 && m.ExpertsUsed < m.Experts
	out, embd := m.OutputBytes, m.EmbdBytes
	for i, t := range ts {
		var sz int64
		if i+1 < len(ts) {
			sz = int64(ts[i+1].off - t.off)
		} else if size > 0 {
			sz = size - dataStart - int64(t.off)
		}
		if sz < 0 {
			sz = 0
		}
		m.Tensors = append(m.Tensors, Tensor{Name: t.name, Offset: dataStart + int64(t.off), Size: sz, Shard: shard})
		if !m.Recurrent && RecurrentTensor(t.name) {
			m.Recurrent = true
		}
		if strings.Contains(t.name, ".nextn.") {
			m.HasMTP = true
			if strings.HasSuffix(t.name, ".eh_proj.weight") {
				m.MTPLayers++
			}
		}
		if l, ok := blockOf(t.name); ok && l < m.Layers {
			m.LayerBytes[l] += sz
			read := float64(sz)
			if m.Experts > 0 && strings.Contains(t.name, "_exps") {
				if m.ExpertBytes == nil {
					m.ExpertBytes = make([]int64, m.Layers)
				}
				m.ExpertBytes[l] += sz
			}
			if moe && strings.Contains(t.name, "_exps") {
				read *= float64(m.ExpertsUsed) / float64(m.Experts)
			}
			m.LayerRead[l] += int64(math.Round(read))
			continue
		}
		m.OtherBytes += sz
		switch t.name {
		case "output.weight":
			out = sz
		case "token_embd.weight":
			embd = sz
		}
	}
	m.EmbdBytes = embd
	m.OutputBytes = out
	if out == 0 { // tied: the output projection is the embedding table
		m.OutputBytes = embd
	}
	m.updateKVBlocks()
}

// updateKVBlocks counts how many blocks keep an attention KV cache when the tensor table mixes attention-KV
// blocks (.attn_k. / .attn_kv / .attn_qkv) and recurrent-only blocks (RecurrentTensor). If only a metadata
// interval (<arch>.full_attention_interval) is present and no recurrent tensors are distinguished, the metadata
// count is kept.
func (m *Model) updateKVBlocks() {
	if m == nil || m.Layers <= 0 || len(m.Tensors) == 0 {
		return
	}
	hasAttn := make([]bool, m.Layers)
	hasRec := make([]bool, m.Layers)
	for _, t := range m.Tensors {
		l, ok := blockOf(t.Name)
		if !ok || l < 0 || l >= m.Layers {
			continue
		}
		if strings.Contains(t.Name, ".attn_k.") || strings.Contains(t.Name, ".attn_kv") || strings.Contains(t.Name, ".attn_qkv") {
			hasAttn[l] = true
		}
		if RecurrentTensor(t.Name) {
			hasRec[l] = true
		}
	}
	attn, recOnly := 0, 0
	for l := 0; l < m.Layers; l++ {
		if hasAttn[l] {
			attn++
		} else if hasRec[l] {
			recOnly++
		}
	}
	if attn > 0 && recOnly > 0 && attn < m.Layers {
		m.KVBlocks = attn
	}
}

// recurrentKeyParts are the GGUF schema key groups (after "<arch>.") that only a block with a recurrent state has.
var recurrentKeyParts = []string{"ssm.", "shortconv.", "wkv.", "rwkv.", "full_attention_interval"}

// RecurrentKeys says whether the integer metadata of arch describes recurrent (state-carrying) blocks.
func RecurrentKeys(arch string, ints map[string]uint64) bool {
	for k := range ints {
		rest, ok := strings.CutPrefix(k, arch+".")
		if !ok {
			continue
		}
		for _, p := range recurrentKeyParts {
			if strings.HasPrefix(rest, p) {
				return true
			}
		}
	}
	return false
}

// RecurrentTensor says whether a tensor name is one of the GGUF schema's recurrent-block tensors.
func RecurrentTensor(name string) bool {
	if _, ok := blockOf(name); !ok {
		return false
	}
	return strings.Contains(name, ".ssm_") || strings.Contains(name, ".shortconv.") || strings.Contains(name, ".time_mix_")
}

// blockOf reads the block number out of "blk.12.attn_q.weight".
func blockOf(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, "blk.")
	if !ok {
		return 0, false
	}
	n, _, ok := strings.Cut(rest, ".")
	if !ok {
		return 0, false
	}
	v, err := strconv.Atoi(n)
	return v, err == nil
}
