// Package ggufhead copies a target model's LM head into a DFlash draft GGUF.
//
// A DFlash2 draft shares the target's output projection by default. In a chain
// the draft runs next to stage 0, which has no LM head, so the draft must carry
// its own copy. The head tensor is
// copied byte for byte: no requantization, the logits stay identical to the
// target's. The draft's metadata and tensors are copied verbatim.
package ggufhead

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

const ggufMagic = 0x46554747 // "GGUF"

// GGUF metadata value types.
const (
	tU8 = iota
	tI8
	tU16
	tI16
	tU32
	tI32
	tF32
	tBool
	tString
	tArray
	tU64
	tI64
	tF64
)

type tensorInfo struct {
	name   string
	ne     []uint64
	typ    uint32
	offset uint64 // relative to the data section
}

type file struct {
	path      string
	version   uint32
	nKV       uint64
	kvStart   int64 // first KV byte
	infoEnd   int64 // byte after the last tensor info
	dataStart int64
	size      int64
	alignment uint64
	strs      map[string]string
	ints      map[string]uint64
	arrLen    map[string]uint64
	tensors   []tensorInfo
}

type counter struct {
	r *bufio.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

type parser struct {
	c   *counter
	err error
}

func (p *parser) full(b []byte) {
	if p.err == nil {
		_, p.err = io.ReadFull(p.c, b)
	}
}

func (p *parser) u32() uint32 {
	var b [4]byte
	p.full(b[:])
	return binary.LittleEndian.Uint32(b[:])
}

func (p *parser) u64() uint64 {
	var b [8]byte
	p.full(b[:])
	return binary.LittleEndian.Uint64(b[:])
}

func (p *parser) str() string {
	n := p.u64()
	if p.err != nil {
		return ""
	}
	if n > 1<<24 {
		p.err = fmt.Errorf("string of %d bytes", n)
		return ""
	}
	b := make([]byte, n)
	p.full(b)
	return string(b)
}

func (p *parser) skip(n uint64) {
	if p.err == nil {
		_, p.err = io.CopyN(io.Discard, p.c, int64(n))
	}
}

var scalarSize = map[uint32]uint64{tU8: 1, tI8: 1, tBool: 1, tU16: 2, tI16: 2, tU32: 4, tI32: 4, tF32: 4, tU64: 8, tI64: 8, tF64: 8}

// value reads one value of type t; integers come back as uint64, strings as string.
func (p *parser) value(t uint32) (any, uint64) {
	switch t {
	case tString:
		return p.str(), 0
	case tArray:
		et := p.u32()
		n := p.u64()
		if sz, ok := scalarSize[et]; ok {
			p.skip(sz * n)
			return nil, n
		}
		if et != tString {
			if p.err == nil {
				p.err = fmt.Errorf("array of type %d", et)
			}
			return nil, n
		}
		for i := uint64(0); i < n && p.err == nil; i++ {
			p.skip(p.u64())
		}
		return nil, n
	}
	sz, ok := scalarSize[t]
	if !ok {
		if p.err == nil {
			p.err = fmt.Errorf("value type %d", t)
		}
		return nil, 0
	}
	var b [8]byte
	p.full(b[:sz])
	return binary.LittleEndian.Uint64(b[:]), 0
}

func alignUp(x, a uint64) uint64 {
	return (x + a - 1) / a * a
}

func parse(path string) (*file, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	p := &parser{c: &counter{r: bufio.NewReaderSize(f, 1<<20)}}
	g := &file{path: path, size: st.Size(), alignment: 32,
		strs: map[string]string{}, ints: map[string]uint64{}, arrLen: map[string]uint64{}}
	if p.u32() != ggufMagic && p.err == nil {
		return nil, fmt.Errorf("%s: not a GGUF file", path)
	}
	g.version = p.u32()
	if p.err == nil && g.version != 2 && g.version != 3 {
		return nil, fmt.Errorf("%s: GGUF version %d not supported", path, g.version)
	}
	nT := p.u64()
	g.nKV = p.u64()
	if p.err == nil && (nT > 1<<20 || g.nKV > 1<<20) {
		return nil, fmt.Errorf("%s: implausible header (%d tensors, %d keys)", path, nT, g.nKV)
	}
	g.kvStart = p.c.n
	for i := uint64(0); i < g.nKV && p.err == nil; i++ {
		k := p.str()
		v, n := p.value(p.u32())
		switch x := v.(type) {
		case string:
			g.strs[k] = x
		case uint64:
			g.ints[k] = x
		case nil:
			g.arrLen[k] = n
		}
	}
	for i := uint64(0); i < nT && p.err == nil; i++ {
		var ti tensorInfo
		ti.name = p.str()
		nd := p.u32()
		if p.err == nil && nd > 8 {
			return nil, fmt.Errorf("%s: tensor %q has %d dims", path, ti.name, nd)
		}
		for d := uint32(0); d < nd; d++ {
			ti.ne = append(ti.ne, p.u64())
		}
		ti.typ = p.u32()
		ti.offset = p.u64()
		g.tensors = append(g.tensors, ti)
	}
	if p.err != nil {
		return nil, fmt.Errorf("%s: %w", path, p.err)
	}
	g.infoEnd = p.c.n
	if a, ok := g.ints["general.alignment"]; ok && a > 0 {
		g.alignment = a
	}
	g.dataStart = int64(alignUp(uint64(g.infoEnd), g.alignment))
	if g.dataStart > g.size {
		return nil, fmt.Errorf("%s: truncated", path)
	}
	return g, nil
}

func (g *file) find(name string) (tensorInfo, bool) {
	for _, t := range g.tensors {
		if t.name == name {
			return t, true
		}
	}
	return tensorInfo{}, false
}

// span is the byte length of tensor t: up to the next tensor or the end of the file (may include alignment padding).
func (g *file) span(t tensorInfo) uint64 {
	end := uint64(g.size - g.dataStart)
	for _, o := range g.tensors {
		if o.offset > t.offset && o.offset < end {
			end = o.offset
		}
	}
	return end - t.offset
}

var shardRe = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})\.gguf$`)

// Shards lists every split of a sharded GGUF given any one of them (a plain file is its own single shard).
func Shards(path string) ([]string, error) {
	m := shardRe.FindStringSubmatch(path)
	if m == nil {
		return []string{path}, nil
	}
	n, _ := strconv.Atoi(m[3])
	var out []string
	for i := 1; i <= n; i++ {
		p := fmt.Sprintf("%s-%05d-of-%s.gguf", m[1], i, m[3])
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("shard %d of %d missing: %w", i, n, err)
		}
		out = append(out, p)
	}
	return out, nil
}

type headPart struct {
	src  *file
	info tensorInfo // as found in the target
	name string     // name in the draft
	n    uint64     // bytes to copy
}

func findHead(shards []string) ([]headPart, error) {
	var out, tied, scale *headPart
	for _, s := range shards {
		g, err := parse(s)
		if err != nil {
			return nil, err
		}
		if t, ok := g.find("output.weight"); ok {
			out = &headPart{src: g, info: t, name: "output.weight", n: g.span(t)}
		}
		if t, ok := g.find("output.scale"); ok {
			scale = &headPart{src: g, info: t, name: "output.scale", n: g.span(t)}
		}
		if t, ok := g.find("token_embd.weight"); ok {
			tied = &headPart{src: g, info: t, name: "output.weight", n: g.span(t)}
		}
	}
	switch {
	case out != nil && scale != nil:
		return []headPart{*out, *scale}, nil
	case out != nil:
		return []headPart{*out}, nil
	case tied != nil: // tied embeddings: the token embedding matrix is the head
		return []headPart{*tied}, nil
	}
	return nil, errors.New("target has neither output.weight nor token_embd.weight")
}

// Result describes what EmbedHead added.
type Result struct {
	Tensors []string // names added to the draft
	Bytes   uint64   // head bytes copied
}

// EmbedHead writes out = draft + the LM head of target (any shard of it). out must differ from both inputs.
func EmbedHead(draftPath, targetPath, outPath string) (Result, error) {
	var res Result
	d, err := parse(draftPath)
	if err != nil {
		return res, err
	}
	if a := d.strs["general.architecture"]; a != "dflash" {
		return res, fmt.Errorf("%s: architecture %q, not a DFlash draft", draftPath, a)
	}
	if _, ok := d.find("output.weight"); ok {
		return res, fmt.Errorf("%s: the draft already has output.weight", draftPath)
	}
	if _, ok := d.find("d2t"); ok {
		return res, fmt.Errorf("%s: reduced-vocabulary draft (d2t): it needs its own head, not the target's", draftPath)
	}
	shards, err := Shards(targetPath)
	if err != nil {
		return res, err
	}
	head, err := findHead(shards)
	if err != nil {
		return res, err
	}
	w := head[0].info
	if len(w.ne) != 2 {
		return res, fmt.Errorf("target head has %d dims", len(w.ne))
	}
	if e, ok := d.ints["dflash.embedding_length"]; ok && w.ne[0] != e {
		return res, fmt.Errorf("target head width %d != draft n_embd %d", w.ne[0], e)
	}
	if v, ok := d.arrLen["tokenizer.ggml.tokens"]; ok && w.ne[1] != v {
		return res, fmt.Errorf("target head rows %d != draft vocabulary %d", w.ne[1], v)
	}
	if abs(outPath) == abs(draftPath) || abs(outPath) == abs(targetPath) {
		return res, errors.New("output must be a new file")
	}

	tmp := outPath + ".tmp"
	if err := write(d, head, tmp); err != nil {
		os.Remove(tmp)
		return res, err
	}
	if err := os.Rename(tmp, outPath); err != nil {
		os.Remove(tmp)
		return res, err
	}
	for _, h := range head {
		res.Tensors = append(res.Tensors, h.name)
		res.Bytes += h.n
	}
	return res, nil
}

// HasHead reports whether the GGUF file at draftPath already contains an output projection tensor ("output.weight").
func HasHead(draftPath string) (bool, error) {
	d, err := parse(draftPath)
	if err != nil {
		return false, err
	}
	_, ok := d.find("output.weight")
	return ok, nil
}

func abs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

func put32(w io.Writer, v uint32) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	_, err := w.Write(b[:])
	return err
}

func put64(w io.Writer, v uint64) error {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	_, err := w.Write(b[:])
	return err
}

func pad(w io.Writer, n int64) error {
	if n <= 0 {
		return nil
	}
	_, err := w.Write(make([]byte, n))
	return err
}

func copyRange(w io.Writer, path string, off, n int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	got, err := io.Copy(w, io.NewSectionReader(f, off, n))
	if err == nil && got != n {
		err = fmt.Errorf("%s: short read (%d of %d bytes)", path, got, n)
	}
	return err
}

func write(d *file, head []headPart, outPath string) (err error) {
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	bw := bufio.NewWriterSize(f, 4<<20)
	cw := &countWriter{w: bw}

	// head tensors go after the draft's data, each aligned
	draftData := uint64(d.size - d.dataStart)
	offs := make([]uint64, len(head))
	next := alignUp(draftData, d.alignment)
	for i, h := range head {
		offs[i] = next
		next = alignUp(next+h.n, d.alignment)
	}
	order := make([]int, len(head))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return offs[order[a]] < offs[order[b]] })

	steps := []func() error{
		func() error { return put32(cw, ggufMagic) },
		func() error { return put32(cw, d.version) },
		func() error { return put64(cw, uint64(len(d.tensors)+len(head))) },
		func() error { return put64(cw, d.nKV) },
		// KV and the draft's tensor infos, verbatim
		func() error { return copyRange(cw, d.path, d.kvStart, d.infoEnd-d.kvStart) },
	}
	for _, s := range steps {
		if err := s(); err != nil {
			return err
		}
	}
	for i, h := range head {
		if err := put64(cw, uint64(len(h.name))); err != nil {
			return err
		}
		if _, err := cw.Write([]byte(h.name)); err != nil {
			return err
		}
		if err := put32(cw, uint32(len(h.info.ne))); err != nil {
			return err
		}
		for _, n := range h.info.ne {
			if err := put64(cw, n); err != nil {
				return err
			}
		}
		if err := put32(cw, h.info.typ); err != nil {
			return err
		}
		if err := put64(cw, offs[i]); err != nil {
			return err
		}
	}
	dataStart := int64(alignUp(uint64(cw.n), d.alignment))
	if err := pad(cw, dataStart-cw.n); err != nil {
		return err
	}
	if err := copyRange(cw, d.path, d.dataStart, int64(draftData)); err != nil {
		return err
	}
	for _, i := range order {
		h := head[i]
		if err := pad(cw, dataStart+int64(offs[i])-cw.n); err != nil {
			return err
		}
		if err := copyRange(cw, h.src.path, h.src.dataStart+int64(h.info.offset), int64(h.n)); err != nil {
			return err
		}
	}
	return bw.Flush()
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
