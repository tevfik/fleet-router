// Package store keeps track of model files: which GGUF files a machine has (identified by the SHA-256 of their
// content), a manifest of each (chunk hashes, where every tensor sits in the file, the hash a llama-ring worker
// caches it under), and what the machine's worker cache already holds. Fleets exchange the inventory, so a
// plan knows who has what. See docs/distributed.md, "Model store".
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tevfik/fleet-router/internal/ggufmeta"
	"github.com/tevfik/fleet-router/internal/plan"
)

// ChunkSize is the unit of verification and (later) transfer: a worker needing a block's bytes takes the chunks
// that overlap them, so the waste at the two edges stays small.
var ChunkSize = 16 << 20

// CacheThreshold: llama-ring caches a tensor in the worker's cache only when it is larger than this
// (HASH_THRESHOLD in its RPC code); smaller ones are always sent.
var CacheThreshold int64 = 10 << 20

// Tensor is one tensor's place in the file and, for the ones a worker caches, the name the cache gives it.
type Tensor struct {
	Name   string `json:"name"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
	FNV    string `json:"fnv,omitempty"` // FNV-1a 64 of the raw bytes, 16 hex digits (the worker cache's file name)
}

// Manifest describes one file completely enough to fetch, verify and cache parts of it elsewhere.
type Manifest struct {
	SHA256    string         `json:"sha256"`
	Size      int64          `json:"size"`
	ChunkSize int64          `json:"chunk_size"`
	Chunks    []string       `json:"chunks"` // SHA-256 of each chunk
	Name      string         `json:"name"`
	Arch      string         `json:"arch"`
	Layers    int            `json:"layers"`
	Tensors   []Tensor       `json:"tensors"`
	Origin    string         `json:"origin,omitempty"`
	BuiltAt   time.Time      `json:"built_at"`
	Meta      *ggufmeta.Meta `json:"meta,omitempty"` // what the GGUF header says (absent in manifests built before it was read)
}

// Entry is a file in the inventory (no path: that is nobody else's business).
type Entry struct {
	SHA256   string `json:"sha256"`
	Name     string `json:"name"`
	Arch     string `json:"arch,omitempty"`
	Layers   int    `json:"layers,omitempty"`
	Size     int64  `json:"size"`
	Complete bool   `json:"complete"` // the file is there and unchanged
	// Kind says what the file is, from the GGUF's own architecture: "mmproj" (a vision projector, arch clip) or
	// "" (a model). A projector is not a model: it only works next to the model it was made for.
	Kind string `json:"kind,omitempty"`
	// From the GGUF header (see ggufmeta): quantization, trained context, experts, and the capabilities the header
	// implies (chat template → chat; its text knowing tools / thinking; pooling → embed). Facts of the file, not a test.
	Quant   string   `json:"quant,omitempty"`
	Ctx     int      `json:"ctx,omitempty"`
	Experts int      `json:"experts,omitempty"`
	Caps    []string `json:"caps,omitempty"`
	// Shard / ShardOf: this file is shard Shard (1-based) of a model split in ShardOf files (split.no / split.count of
	// the header; the name need not say so).
	Shard   int `json:"shard,omitempty"`
	ShardOf int `json:"shard_of,omitempty"`
}

// CacheInfo is a worker cache directory: what it holds, by the names the cache gives.
type CacheInfo struct {
	Dir     string           `json:"dir"`
	Bytes   int64            `json:"bytes"`
	Entries int              `json:"entries"`
	Files   map[string]int64 `json:"files,omitempty"` // name → size (the first maxCacheFiles)
}

const maxCacheFiles = 20000

// Progress of the hashing that is running.
type Progress struct {
	Path  string `json:"path"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
}

// Inventory is what a machine has.
type Inventory struct {
	Files     []Entry    `json:"files"`
	Cache     *CacheInfo `json:"cache,omitempty"`
	Hashing   *Progress  `json:"hashing,omitempty"`
	Transfers []Transfer `json:"transfers,omitempty"` // pulls and cache fills, running first, then the latest finished
	Error     string     `json:"error,omitempty"`     // why the last add failed
	Notes     []string   `json:"notes,omitempty"`     // what the last add found out (an Ollama model with a projector, …)
}

type record struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
	SHA   string `json:"sha256"`
}

// Store is the registry of one machine's model files, kept in a directory of its own.
type Store struct {
	dir    string
	mu     sync.Mutex
	rec    []record
	ok     bool // index loaded
	prg    *Progress
	err    string // the last failed add
	trs    []*Transfer
	nextID int
}

// Open returns the store kept in dir (created when something is added).
func Open(dir string) *Store { return &Store{dir: dir} }

func (s *Store) indexPath() string              { return filepath.Join(s.dir, "index.json") }
func (s *Store) manifestPath(sha string) string { return filepath.Join(s.dir, sha+".manifest.json") }

func (s *Store) load() {
	if s.ok {
		return
	}
	s.ok = true
	if b, err := os.ReadFile(s.indexPath()); err == nil {
		json.Unmarshal(b, &s.rec)
	}
}

func (s *Store) save() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s.rec, "", "  ")
	return writeAtomic(s.indexPath(), b)
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Add registers a file: it is hashed (one pass over the whole file: its SHA-256, the SHA-256 of every chunk and
// the cache hash of every large tensor) unless it is already known unchanged. Only one file is hashed at a time.
func (s *Store) Add(ctx context.Context, path string) (Entry, error) {
	return s.AddNamed(ctx, path, "")
}

// AddNamed is Add under a name of the caller's: a file whose own name says nothing (an Ollama blob is called
// sha256-…) is listed by the name its manifest gives it. "" keeps the name the GGUF carries.
func (s *Store) AddNamed(ctx context.Context, path, name string) (Entry, error) {
	e, err := s.add(ctx, path, name)
	s.mu.Lock()
	s.err = ""
	if err != nil {
		s.err = filepath.Base(path) + ": " + err.Error()
	}
	s.mu.Unlock()
	return e, err
}

func (s *Store) add(ctx context.Context, path, name string) (Entry, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return Entry{}, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return Entry{}, err
	}
	if st.IsDir() {
		return Entry{}, fmt.Errorf("%s is a directory", path)
	}
	s.mu.Lock()
	s.load()
	for _, r := range s.rec {
		if r.Path == path && r.Size == st.Size() && r.MTime == st.ModTime().UnixNano() {
			s.mu.Unlock()
			if m, err := s.Manifest(r.SHA); err == nil {
				if name != "" && m.Name != name { // listed under another name now
					m.Name = name
					b, _ := json.Marshal(m)
					_ = writeAtomic(s.manifestPath(m.SHA256), b)
				}
				return entryOf(m, true), nil
			}
			s.mu.Lock() // the manifest is gone: hash again
			break
		}
	}
	if s.prg != nil {
		s.mu.Unlock()
		return Entry{}, fmt.Errorf("already hashing %s", filepath.Base(s.prg.Path))
	}
	s.prg = &Progress{Path: path, Total: st.Size()}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.prg = nil; s.mu.Unlock() }()

	m, err := s.build(ctx, path, st.Size())
	if err != nil {
		return Entry{}, err
	}
	if name != "" {
		m.Name = name
	}
	b, _ := json.Marshal(m)
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return Entry{}, err
	}
	if err := writeAtomic(s.manifestPath(m.SHA256), b); err != nil {
		return Entry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := record{Path: path, Size: st.Size(), MTime: st.ModTime().UnixNano(), SHA: m.SHA256}
	replaced := false
	for i := range s.rec {
		if s.rec[i].Path == path {
			s.rec[i], replaced = rec, true
		}
	}
	if !replaced {
		s.rec = append(s.rec, rec)
	}
	return entryOf(m, true), s.save()
}

var triedMeta sync.Map // sha → true: a header that could not be read is not tried again on every inventory

func (m *Manifest) metaTried() bool {
	_, loaded := triedMeta.LoadOrStore(m.SHA256, true)
	return loaded
}

func entryOf(m *Manifest, complete bool) Entry {
	e := Entry{SHA256: m.SHA256, Name: m.Name, Arch: m.Arch, Layers: m.Layers, Size: m.Size, Complete: complete}
	if m.Arch == "clip" {
		e.Kind = "mmproj"
	}
	if g := m.Meta; g != nil {
		e.Quant, e.Ctx, e.Experts, e.Caps = g.Quant, g.Ctx, g.Experts, g.Caps()
		if g.Shards > 1 {
			e.Shard, e.ShardOf = g.ShardNo+1, g.Shards
		}
		if g.Kind() == "mmproj" {
			e.Kind = "mmproj"
		}
	}
	return e
}

// Prune forgets the registered files that are gone or changed (their records would only show as missing); the
// files themselves are never touched. It returns what was forgotten.
func (s *Store) Prune() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	var keep []record
	var gone []Entry
	for _, r := range s.rec {
		st, err := os.Stat(r.Path)
		if err == nil && st.Size() == r.Size && st.ModTime().UnixNano() == r.MTime {
			keep = append(keep, r)
			continue
		}
		if m, err := s.Manifest(r.SHA); err == nil {
			gone = append(gone, entryOf(m, false))
		} else {
			gone = append(gone, Entry{SHA256: r.SHA, Name: filepath.Base(r.Path)})
		}
	}
	if len(gone) == 0 {
		return nil, nil
	}
	s.rec = keep
	for _, e := range gone {
		still := false
		for _, o := range s.rec {
			still = still || o.SHA == e.SHA256
		}
		if !still {
			os.Remove(s.manifestPath(e.SHA256))
		}
	}
	return gone, s.save()
}

// DeleteFile removes a registered file from the disk, but only when it lies inside dir (what fleet fetched itself:
// a file the user keeps elsewhere is never deleted by fleet), and forgets it. It returns the path it removed.
func (s *Store) DeleteFile(ref, dir string) (Entry, string, error) {
	s.mu.Lock()
	s.load()
	var path string
	for _, r := range s.rec {
		m, err := s.Manifest(r.SHA)
		if err == nil && (strings.HasPrefix(r.SHA, strings.ToLower(ref)) || m.Name == ref || filepath.Base(r.Path) == ref) {
			path = r.Path
		}
	}
	s.mu.Unlock()
	if path == "" {
		return Entry{}, "", fmt.Errorf("no file in the store matches %q", ref)
	}
	rel, err := filepath.Rel(dir, path)
	if dir == "" || err != nil || strings.HasPrefix(rel, "..") {
		return Entry{}, "", fmt.Errorf("%s is not in %s, the directory fleet fetches into: fleet only deletes what it fetched itself (forget it with fleet store rm and delete the file yourself)", path, dir)
	}
	e, err := s.Remove(ref)
	if err != nil {
		return e, "", err
	}
	return e, path, os.Remove(path)
}

func (s *Store) progress(done int64) {
	s.mu.Lock()
	if s.prg != nil {
		s.prg.Done = done
	}
	s.mu.Unlock()
}

const (
	fnvOffset = 0xcbf29ce484222325
	fnvPrime  = 0x100000001b3
)

func fnvUpdate(h uint64, b []byte) uint64 {
	for _, c := range b {
		h ^= uint64(c)
		h *= fnvPrime
	}
	return h
}

// FNVHex is the name llama-ring's worker cache gives a tensor: FNV-1a 64 of its bytes, 16 hex digits.
func FNVHex(b []byte) string { return fmt.Sprintf("%016x", fnvUpdate(fnvOffset, b)) }

// build reads the file once, block by block (a block is a chunk): the three hashes are worked out side by side.
func (s *Store) build(ctx context.Context, path string, size int64) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	mod, err := plan.ReadGGUF(path)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Size: size, ChunkSize: int64(ChunkSize), Name: mod.Name, Arch: mod.Arch, Layers: mod.Layers, BuiltAt: time.Now().UTC()}
	if g, err := ggufmeta.Read(path); err == nil {
		m.Meta = g
	}
	ts := append([]plan.Tensor{}, mod.Tensors...)
	sort.Slice(ts, func(i, j int) bool { return ts[i].Offset < ts[j].Offset })
	fnv := make([]uint64, len(ts))
	for i, t := range ts {
		m.Tensors = append(m.Tensors, Tensor{Name: t.Name, Offset: t.Offset, Size: t.Size})
		fnv[i] = fnvOffset
	}
	whole := sha256.New()
	buf := make([]byte, ChunkSize)
	first := 0 // the first tensor that may still overlap the next block
	for pos := int64(0); pos < size; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := io.ReadFull(f, buf)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if n == 0 {
			break
		}
		blk := buf[:n]
		var chunk [sha256.Size]byte
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); chunk = sha256.Sum256(blk) }()
		go func() { defer wg.Done(); whole.Write(blk) }()
		go func() {
			defer wg.Done()
			end := pos + int64(n)
			for first < len(ts) && ts[first].Offset+ts[first].Size <= pos {
				first++
			}
			for i := first; i < len(ts) && ts[i].Offset < end; i++ {
				if ts[i].Size <= CacheThreshold {
					continue
				}
				lo, hi := max(ts[i].Offset, pos), min(ts[i].Offset+ts[i].Size, end)
				if hi > lo {
					fnv[i] = fnvUpdate(fnv[i], blk[lo-pos:hi-pos])
				}
			}
		}()
		wg.Wait()
		m.Chunks = append(m.Chunks, hex.EncodeToString(chunk[:]))
		pos += int64(n)
		s.progress(pos)
	}
	for i := range m.Tensors {
		if m.Tensors[i].Size > CacheThreshold {
			m.Tensors[i].FNV = fmt.Sprintf("%016x", fnv[i])
		}
	}
	m.SHA256 = hex.EncodeToString(whole.Sum(nil))
	return m, nil
}

// Manifest returns the stored manifest of a file.
func (s *Store) Manifest(sha string) (*Manifest, error) {
	b, err := os.ReadFile(s.manifestPath(sha))
	if err != nil {
		return nil, err
	}
	var m Manifest
	return &m, json.Unmarshal(b, &m)
}

// Remove forgets a file (by SHA-256 prefix or by name): its manifest goes, the file itself is never touched.
func (s *Store) Remove(ref string) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	var hit []int
	for i, r := range s.rec {
		// a sha matches even when the manifest is gone or damaged: such a record must still be forgettable
		if strings.HasPrefix(r.SHA, strings.ToLower(ref)) {
			hit = append(hit, i)
			continue
		}
		if m, err := s.Manifest(r.SHA); err == nil && (m.Name == ref || filepath.Base(r.Path) == ref) {
			hit = append(hit, i)
		}
	}
	switch len(hit) {
	case 0:
		return Entry{}, fmt.Errorf("no file in the store matches %q", ref)
	case 1:
	default:
		return Entry{}, fmt.Errorf("%q matches %d files: give more of the sha256", ref, len(hit))
	}
	r := s.rec[hit[0]]
	m, _ := s.Manifest(r.SHA)
	s.rec = append(s.rec[:hit[0]], s.rec[hit[0]+1:]...)
	still := false
	for _, o := range s.rec {
		still = still || o.SHA == r.SHA
	}
	if !still {
		os.Remove(s.manifestPath(r.SHA))
	}
	e := Entry{SHA256: r.SHA}
	if m != nil {
		e = entryOf(m, false)
	}
	return e, s.save()
}

// Inventory lists the registered files (complete only while they are still there, unchanged) and the cache.
func (s *Store) Inventory(cacheDir string) Inventory {
	s.mu.Lock()
	s.load()
	recs := append([]record{}, s.rec...)
	var prg *Progress
	if s.prg != nil {
		p := *s.prg
		prg = &p
	}
	lastErr := s.err
	trs := s.transfersLocked()
	s.mu.Unlock()
	inv := Inventory{Files: []Entry{}, Hashing: prg, Error: lastErr, Transfers: trs}
	seen := map[string]bool{}
	for _, r := range recs {
		m, err := s.Manifest(r.SHA)
		if err != nil {
			continue
		}
		st, err := os.Stat(r.Path)
		ok := err == nil && st.Size() == r.Size && st.ModTime().UnixNano() == r.MTime
		if ok && m.Meta == nil && !m.metaTried() { // a manifest built before the header was read: read it once now
			if g, err := ggufmeta.Read(r.Path); err == nil {
				m.Meta = g
				if b, err := json.Marshal(m); err == nil {
					_ = writeAtomic(s.manifestPath(m.SHA256), b)
				}
			}
		}
		if seen[r.SHA] && !ok {
			continue
		}
		if seen[r.SHA] { // the same content under two paths: one entry, complete if either is
			for i := range inv.Files {
				if inv.Files[i].SHA256 == r.SHA {
					inv.Files[i].Complete = true
				}
			}
			continue
		}
		seen[r.SHA] = true
		inv.Files = append(inv.Files, entryOf(m, ok))
	}
	sort.Slice(inv.Files, func(i, j int) bool { return inv.Files[i].Name < inv.Files[j].Name })
	if cacheDir != "" {
		inv.Cache = ScanCache(cacheDir)
	}
	return inv
}

// Path is where a registered file is on this machine ("" when it is not there any more).
func (s *Store) Path(sha string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	for _, r := range s.rec {
		if r.SHA == sha {
			if st, err := os.Stat(r.Path); err == nil && st.Size() == r.Size && st.ModTime().UnixNano() == r.MTime {
				return r.Path
			}
		}
	}
	return ""
}

// Find returns the manifest of the registered file with this size (and name, when several share the size).
func (s *Store) Find(size int64, name string) *Manifest {
	s.mu.Lock()
	s.load()
	recs := append([]record{}, s.rec...)
	s.mu.Unlock()
	var best *Manifest
	for _, r := range recs {
		if r.Size != size {
			continue
		}
		m, err := s.Manifest(r.SHA)
		if err != nil {
			continue
		}
		if m.Name == name {
			return m
		}
		if best == nil {
			best = m
		}
	}
	return best
}

// CacheDir is where llama-ring workers keep their weights on this machine: $LLAMA_CACHE/rpc ("" when unset).
func CacheDir() string {
	if d := os.Getenv("LLAMA_CACHE"); d != "" {
		return filepath.Join(d, "rpc")
	}
	return ""
}

// ScanCache lists a worker cache directory (nil when it does not exist).
func ScanCache(dir string) *CacheInfo {
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	c := &CacheInfo{Dir: dir, Files: map[string]int64{}}
	for _, e := range es {
		if e.IsDir() || !isCacheName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		c.Entries++
		c.Bytes += info.Size()
		if len(c.Files) < maxCacheFiles {
			c.Files[e.Name()] = info.Size()
		}
	}
	return c
}

// isCacheName: a worker cache file is named by 16 lower-case hex digits (a temp file of a fill in progress is not).
func isCacheName(n string) bool {
	if len(n) != 16 {
		return false
	}
	for _, c := range n {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// CacheHit says how many bytes of the file's cacheable tensors a worker cache already holds (a tensor counts
// when a cache entry of its name and size exists) out of how many there are.
func (m *Manifest) CacheHit(files map[string]int64) (have, total int64) {
	for _, t := range m.Tensors {
		if t.FNV == "" {
			continue
		}
		total += t.Size
		if sz, ok := files[t.FNV]; ok && sz == t.Size {
			have += t.Size
		}
	}
	return have, total
}

// KnownSHA is the SHA-256 the store has on record for the file at path, when the file is unchanged since (same size
// and modification time); "" otherwise. It never reads the file.
func (s *Store) KnownSHA(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	st, err := os.Stat(abs)
	if err != nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	for _, r := range s.rec {
		if r.Path == abs && r.Size == st.Size() && r.MTime == st.ModTime().UnixNano() {
			return r.SHA
		}
	}
	return ""
}
