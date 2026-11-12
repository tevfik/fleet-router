package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Moving model files between fleets, and filling a worker's cache from another fleet's file. Every byte is
// checked against the chunk hashes of the manifest before it is kept; a transfer resumes where it stopped; a
// bandwidth limit keeps it from starving inference; free disk space is checked first.

// Transfer is a pull or a cache fill, running or finished.
type Transfer struct {
	ID       int       `json:"id"`
	Kind     string    `json:"kind"` // "pull" | "fill"
	What     string    `json:"what"`
	Source   string    `json:"source"`
	Done     int64     `json:"done"`
	Total    int64     `json:"total"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
	Err      string    `json:"err,omitempty"`
	Running  bool      `json:"running"`
}

type handle struct {
	s    *Store
	t    *Transfer
	done atomic.Int64
}

// begin registers a transfer; Add counts bytes, end finishes it.
func (s *Store) begin(kind, what, source string, total int64) *handle {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	t := &Transfer{ID: s.nextID, Kind: kind, What: what, Source: source, Total: total, Started: time.Now(), Running: true}
	s.trs = append(s.trs, t)
	if len(s.trs) > 20 { // keep the latest ones
		var keep []*Transfer
		for _, x := range s.trs {
			if x.Running {
				keep = append(keep, x)
			}
		}
		for i := len(s.trs) - 1; i >= 0 && len(keep) < 8; i-- {
			if !s.trs[i].Running {
				keep = append([]*Transfer{s.trs[i]}, keep...)
			}
		}
		s.trs = keep
	}
	return &handle{s: s, t: t}
}

func (h *handle) add(n int64) {
	v := h.done.Add(n)
	h.s.mu.Lock()
	h.t.Done = v
	h.s.mu.Unlock()
}

func (h *handle) end(err error) {
	h.s.mu.Lock()
	h.t.Running, h.t.Finished = false, time.Now()
	if err != nil {
		h.t.Err = err.Error()
	}
	h.s.mu.Unlock()
}

func (s *Store) transfersLocked() []Transfer {
	var out []Transfer
	for _, t := range s.trs {
		out = append(out, *t)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Running != out[j].Running {
			return out[i].Running
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// ClearTransfers forgets the finished transfers (the failed ones show as warnings until then); running ones stay.
// It returns how many were forgotten.
func (s *Store) ClearTransfers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keep []*Transfer
	for _, t := range s.trs {
		if t.Running {
			keep = append(keep, t)
		}
	}
	n := len(s.trs) - len(keep)
	s.trs = keep
	return n
}

// Busy reports whether a transfer is running.
func (s *Store) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.trs {
		if t.Running {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- serving

// ManifestHandler serves the manifest of a registered file ({sha} in the path); BlobHandler serves its bytes with
// range support. Only complete, registered files are served: nothing else on the disk is reachable.
func (s *Store) ManifestHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sha := r.PathValue("sha")
		if !validSHA(sha) || s.Path(sha) == "" {
			http.Error(w, `{"error":{"message":"no such file in the store"}}`, 404)
			return
		}
		m, err := s.Manifest(sha)
		if err != nil {
			http.Error(w, `{"error":{"message":"no manifest"}}`, 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(m)
	}
}

func (s *Store) BlobHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sha := r.PathValue("sha")
		p := ""
		if validSHA(sha) {
			p = s.Path(sha)
		}
		if p == "" {
			http.Error(w, `{"error":{"message":"no such file in the store"}}`, 404)
			return
		}
		f, err := os.Open(p)
		if err != nil {
			http.Error(w, `{"error":{"message":"the file is not readable"}}`, 404)
			return
		}
		defer f.Close()
		st, _ := f.Stat()
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", st.ModTime(), f)
	}
}

func validSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// ---------------------------------------------------------------- fetching

// Source is another fleet that has the file: its address (http://host:port, no path) and the key to give it.
type Source struct {
	Base string
	Key  string
	HTTP *http.Client
	Name string // for messages
}

// Limit caps the speed of every read of a transfer, in bytes per second (0: none).
type Limit struct {
	mu   sync.Mutex
	bps  int64
	next time.Time
}

func NewLimit(bps int64) *Limit { return &Limit{bps: bps} }

func (l *Limit) wait(ctx context.Context, n int) error {
	if l == nil || l.bps <= 0 {
		return nil
	}
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	d := l.next.Sub(now)
	l.next = l.next.Add(time.Duration(float64(n) / float64(l.bps) * float64(time.Second)))
	l.mu.Unlock()
	if d <= 0 {
		return nil
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (src Source) client() *http.Client {
	if src.HTTP != nil {
		return src.HTTP
	}
	return http.DefaultClient
}

func (src Source) get(ctx context.Context, path, rng string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(src.Base, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	if src.Key != "" {
		req.Header.Set("Authorization", "Bearer "+src.Key)
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := src.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("%s answered %d: %s", orStr(src.Name, src.Base), resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// FetchManifest asks the source for a file's manifest and checks that it is sane and for the file asked for.
func (src Source) FetchManifest(ctx context.Context, sha string) (*Manifest, error) {
	resp, err := src.get(ctx, "/fleet/store/manifest/"+sha, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if err := m.check(); err != nil {
		return nil, err
	}
	if m.SHA256 != sha {
		return nil, fmt.Errorf("the source sent the manifest of another file (%s)", m.SHA256)
	}
	return &m, nil
}

// check rejects a manifest that cannot describe a real file.
func (m *Manifest) check() error {
	if !validSHA(m.SHA256) || m.Size <= 0 || m.ChunkSize < 1<<10 || m.ChunkSize > 1<<30 {
		return errors.New("the manifest is not valid")
	}
	if want := int((m.Size + m.ChunkSize - 1) / m.ChunkSize); len(m.Chunks) != want {
		return fmt.Errorf("the manifest has %d chunk hashes for %d chunks", len(m.Chunks), want)
	}
	for _, c := range m.Chunks {
		if len(c) != 64 {
			return errors.New("the manifest has a malformed chunk hash")
		}
	}
	for _, t := range m.Tensors {
		if t.Offset < 0 || t.Size < 0 || t.Offset+t.Size > m.Size {
			return fmt.Errorf("tensor %q is outside the file", t.Name)
		}
	}
	return nil
}

func (m *Manifest) chunkRange(i int) (off, n int64) {
	off = int64(i) * m.ChunkSize
	return off, min(m.ChunkSize, m.Size-off)
}

// fetchChunk reads chunk i from the source and verifies it against the manifest. Three tries.
func (src Source) fetchChunk(ctx context.Context, m *Manifest, i int, lim *Limit, h *handle) ([]byte, error) {
	off, n := m.chunkRange(i)
	var last error
	for try := 0; try < 3; try++ {
		if try > 0 {
			select {
			case <-time.After(time.Duration(try) * 300 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		b, err := src.readRange(ctx, m.SHA256, off, n, lim)
		if err != nil {
			last = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != m.Chunks[i] {
			last = fmt.Errorf("chunk %d does not match the manifest (corrupt transfer or a different file)", i)
			continue
		}
		return b, nil
	}
	return nil, last
}

func (src Source) readRange(ctx context.Context, sha string, off, n int64, lim *Limit) ([]byte, error) {
	resp, err := src.get(ctx, "/fleet/store/blob/"+sha, fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent && !(resp.StatusCode == 200 && off == 0) {
		return nil, fmt.Errorf("the source ignored the range request (status %d)", resp.StatusCode)
	}
	buf := make([]byte, n)
	for got := 0; got < len(buf); {
		step := min(len(buf)-got, 256<<10)
		k, err := io.ReadFull(resp.Body, buf[got:got+step])
		if k > 0 {
			if werr := lim.wait(ctx, k); werr != nil {
				return nil, werr
			}
		}
		got += k
		if err != nil {
			if got == len(buf) {
				break
			}
			return nil, err
		}
	}
	return buf, nil
}

// FreeBytes is the space available on the filesystem of dir.
func FreeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize)), nil
}

// ---------------------------------------------------------------- pulling a whole file

// PullOptions of a pull or a fill.
type PullOptions struct {
	Parallel int    // chunks in flight (default 4)
	Limit    *Limit // nil: no limit
}

type partMap struct {
	Done []int `json:"done"`
}

// Pull copies a file from another fleet into dir and registers it. A partial copy (name.part and name.part.map)
// is continued; every chunk is verified before it counts; the finished file's SHA-256 is checked once more.
func (s *Store) Pull(ctx context.Context, src Source, sha, dir string, o PullOptions) (Entry, error) {
	if !validSHA(sha) {
		return Entry{}, errors.New("give the full sha256 of the file")
	}
	if p := s.Path(sha); p != "" {
		m, err := s.Manifest(sha)
		if err == nil {
			return entryOf(m, true), nil // already here
		}
	}
	m, err := src.FetchManifest(ctx, sha)
	if err != nil {
		return Entry{}, err
	}
	h := s.begin("pull", m.Name, orStr(src.Name, src.Base), m.Size)
	e, err := s.pull(ctx, src, m, dir, o, h)
	h.end(err)
	return e, err
}

func (s *Store) pull(ctx context.Context, src Source, m *Manifest, dir string, o PullOptions, h *handle) (Entry, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Entry{}, err
	}
	final := filepath.Join(dir, safeName(m.Name)+"-"+m.SHA256[:8]+".gguf")
	part, mapPath := final+".part", final+".part.map"
	done := map[int]bool{}
	if b, err := os.ReadFile(mapPath); err == nil {
		var pm partMap
		if json.Unmarshal(b, &pm) == nil {
			for _, i := range pm.Done {
				if i >= 0 && i < len(m.Chunks) {
					done[i] = true
				}
			}
		}
	}
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return Entry{}, err
	}
	defer f.Close()
	if st, _ := f.Stat(); st.Size() != m.Size { // a new or a mismatched part: start over
		done = map[int]bool{}
		if err := f.Truncate(0); err != nil {
			return Entry{}, err
		}
		if err := f.Truncate(m.Size); err != nil {
			return Entry{}, err
		}
	}
	var have int64
	for i := range done {
		_, n := m.chunkRange(i)
		have += n
	}
	if free, err := FreeBytes(dir); err == nil && free < m.Size-have {
		return Entry{}, fmt.Errorf("not enough disk space in %s: %s needed, %s free", dir, human(m.Size-have), human(free))
	}
	h.add(have)

	par := o.Parallel
	if par <= 0 {
		par = 4
	}
	var mu sync.Mutex
	save := func() {
		var l []int
		for i := range done {
			l = append(l, i)
		}
		sort.Ints(l)
		b, _ := json.Marshal(partMap{Done: l})
		writeAtomic(mapPath, b)
	}
	todo := make(chan int)
	errc := make(chan error, par)
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for w := 0; w < par; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range todo {
				b, err := src.fetchChunk(ctx2, m, i, o.Limit, h)
				if err == nil {
					off, _ := m.chunkRange(i)
					_, err = f.WriteAt(b, off)
				}
				if err != nil {
					select {
					case errc <- err:
					default:
					}
					cancel()
					return
				}
				h.add(int64(len(b)))
				mu.Lock()
				done[i] = true
				if len(done)%16 == 0 {
					save()
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for i := range m.Chunks {
		mu.Lock()
		skip := done[i]
		mu.Unlock()
		if skip {
			continue
		}
		select {
		case todo <- i:
		case <-ctx2.Done():
			break feed
		}
	}
	close(todo)
	wg.Wait()
	mu.Lock()
	save()
	mu.Unlock()
	select {
	case err := <-errc:
		return Entry{}, err
	default:
	}
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	// every chunk matched its hash; the whole file must match too (the manifest's claim about itself)
	if err := f.Sync(); err != nil {
		return Entry{}, err
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, io.NewSectionReader(f, 0, m.Size)); err != nil {
		return Entry{}, err
	}
	if hex.EncodeToString(sum.Sum(nil)) != m.SHA256 {
		os.Remove(part)
		os.Remove(mapPath)
		return Entry{}, errors.New("the finished file does not match its sha256: the manifest was wrong, or the file changed on the source")
	}
	f.Close()
	if err := os.Rename(part, final); err != nil {
		return Entry{}, err
	}
	os.Remove(mapPath)
	return s.register(final, m)
}

// register records a verified file with its manifest, without hashing it again.
func (s *Store) register(path string, m *Manifest) (Entry, error) {
	st, err := os.Stat(path)
	if err != nil {
		return Entry{}, err
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
	s.load()
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

func safeName(n string) string {
	var b strings.Builder
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "model"
	}
	return b.String()
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + " GiB"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MiB"
	}
	return strconv.FormatInt(n, 10) + " B"
}

// ---------------------------------------------------------------- filling a worker cache

// BlockSet says which tensors a worker needs: some blocks, and the output tensors when it holds the output layer.
type BlockSet struct {
	Blocks []int
	Output bool
}

// ParseBlocks reads "0-3,7,10-12" (inclusive ranges).
func ParseBlocks(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil || a < 0 {
			return nil, fmt.Errorf("%q is not a block number or range", part)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil || b < a {
				return nil, fmt.Errorf("%q is not a block range", part)
			}
		}
		if b-a > 4096 {
			return nil, fmt.Errorf("%q is too long a range", part)
		}
		for i := a; i <= b; i++ {
			out = append(out, i)
		}
	}
	return out, nil
}

// Select returns the cacheable tensors (those the worker cache holds) of the set's blocks, plus the output
// tensors (every large tensor outside the blocks except the token embedding, which stays on the master).
func (m *Manifest) Select(set BlockSet) []Tensor {
	want := map[int]bool{}
	for _, b := range set.Blocks {
		want[b] = true
	}
	var out []Tensor
	for _, t := range m.Tensors {
		if t.FNV == "" {
			continue
		}
		if n, ok := blockNumber(t.Name); ok {
			if want[n] {
				out = append(out, t)
			}
			continue
		}
		if set.Output && t.Name != "token_embd.weight" {
			out = append(out, t)
		}
	}
	return out
}

func blockNumber(name string) (int, bool) {
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

// FillResult says what a cache fill did.
type FillResult struct {
	Tensors int   `json:"tensors"` // wanted
	Present int   `json:"present"` // already in the cache
	Written int   `json:"written"`
	Bytes   int64 `json:"bytes"` // fetched
}

// FillCache writes the cache entries of the wanted tensors that are missing: the chunks that cover a tensor are
// fetched and verified against the manifest, the tensor is cut out, its cache name is computed and must match the
// manifest's, and the file appears atomically (temp file + rename).
func (s *Store) FillCache(ctx context.Context, src Source, sha, cacheDir string, set BlockSet, o PullOptions) (FillResult, error) {
	var res FillResult
	if !validSHA(sha) {
		return res, errors.New("give the full sha256 of the file")
	}
	if cacheDir == "" {
		return res, errors.New("no worker cache directory: set LLAMA_CACHE for the daemon or ring_cache in the configuration")
	}
	m, err := src.FetchManifest(ctx, sha)
	if err != nil {
		return res, err
	}
	want := m.Select(set)
	if len(want) == 0 {
		return res, errors.New("no cacheable tensor in those blocks (the model has fewer blocks, or its tensors are small)")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return res, err
	}
	var todo []Tensor
	var total int64
	for _, t := range want {
		res.Tensors++
		if st, err := os.Stat(filepath.Join(cacheDir, t.FNV)); err == nil && st.Size() == t.Size {
			res.Present++
			continue
		}
		todo = append(todo, t)
		total += t.Size
	}
	if len(todo) == 0 {
		return res, nil
	}
	if free, err := FreeBytes(cacheDir); err == nil && free < total {
		return res, fmt.Errorf("not enough disk space in %s: %s needed, %s free", cacheDir, human(total), human(free))
	}
	h := s.begin("fill", fmt.Sprintf("%s: %d tensors", m.Name, len(todo)), orStr(src.Name, src.Base), total)
	var fetched atomic.Int64
	err = s.fillTensors(ctx, src, m, todo, cacheDir, o, h, &fetched, &res)
	h.end(err)
	res.Bytes = fetched.Load()
	return res, err
}

func (s *Store) fillTensors(ctx context.Context, src Source, m *Manifest, todo []Tensor, dir string, o PullOptions, h *handle, fetched *atomic.Int64, res *FillResult) error {
	par := o.Parallel
	if par <= 0 {
		par = 3
	}
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan Tensor)
	errc := make(chan error, par)
	var wg sync.WaitGroup
	var wmu sync.Mutex
	for w := 0; w < par; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				if err := fillOne(ctx2, src, m, t, dir, o.Limit, h, fetched); err != nil {
					select {
					case errc <- fmt.Errorf("%s: %w", t.Name, err):
					default:
					}
					cancel()
					return
				}
				wmu.Lock()
				res.Written++
				wmu.Unlock()
			}
		}()
	}
loop:
	for _, t := range todo {
		select {
		case jobs <- t:
		case <-ctx2.Done():
			break loop
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case err := <-errc:
		return err
	default:
	}
	return ctx.Err()
}

func fillOne(ctx context.Context, src Source, m *Manifest, t Tensor, dir string, lim *Limit, h *handle, fetched *atomic.Int64) error {
	tmp, err := os.CreateTemp(dir, ".fill-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	defer tmp.Close()
	hash := uint64(fnvOffset)
	first, last := int(t.Offset/m.ChunkSize), int((t.Offset+t.Size-1)/m.ChunkSize)
	for i := first; i <= last; i++ {
		b, err := src.fetchChunk(ctx, m, i, lim, h)
		if err != nil {
			return err
		}
		fetched.Add(int64(len(b)))
		off, _ := m.chunkRange(i)
		lo, hi := max(t.Offset, off), min(t.Offset+t.Size, off+int64(len(b)))
		part := b[lo-off : hi-off]
		hash = fnvUpdate(hash, part)
		if _, err := tmp.Write(part); err != nil {
			return err
		}
		h.add(int64(len(part)))
	}
	if got := fmt.Sprintf("%016x", hash); got != t.FNV {
		return fmt.Errorf("the tensor's cache name is %s, the manifest says %s", got, t.FNV)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	os.Chmod(tmp.Name(), 0o644)
	return os.Rename(tmp.Name(), filepath.Join(dir, t.FNV))
}
