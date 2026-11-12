package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/store"
)

// Moving model files between fleets (docs/distributed.md, "Model store"): a fleet serves the files it has
// (manifest and byte ranges, to paired fleets and API-key holders), and pulls what it lacks from a fleet that has
// it, or fills its llama-ring worker cache with just the tensors of the blocks it will hold. Starting a transfer
// is an admin call on the machine that receives; from another machine it is only forwarded over a route that
// is trusted as admin there (SSH), never over a peer key.

// PullRequest asks a machine to fetch a file.
type PullRequest struct {
	Node string `json:"node,omitempty"` // the machine that receives it (default: this one)
	From string `json:"from,omitempty"` // the machine to take it from (default: any that has it)
	SHA  string `json:"sha256"`
	Dir  string `json:"dir,omitempty"` // where to put it (default: the store's files directory)
}

// FillRequest asks a machine to fill its worker cache with some blocks of a model.
type FillRequest struct {
	Node   string `json:"node,omitempty"`
	From   string `json:"from,omitempty"`
	SHA    string `json:"sha256"`
	Blocks string `json:"blocks"` // "0-3,7"
	Output bool   `json:"output,omitempty"`
}

// AbortRequest asks a machine to abort any active file pull or worker cache fill.
type AbortRequest struct {
	Node string `json:"node,omitempty"`
	// Discard also deletes the unfinished copies (name.gguf.part and .part.map) in the store's files directory and
	// forgets the finished transfers, so no warning is left. Nothing running is not an error then.
	Discard bool `json:"discard,omitempty"`
}

// DiscardPartials stops a running transfer, deletes the unfinished copies of pulled files and forgets the
// finished transfers. Registered (complete) files are never touched. It returns what it removed.
func (s *Server) DiscardPartials() (removed []string, cleared int) {
	if s.AbortTransfer() {
		for i := 0; i < 100 && s.Files().Busy(); i++ { // the writer must be gone before its file is
			time.Sleep(50 * time.Millisecond)
		}
	}
	dir := s.filesDir()
	for _, pat := range []string{"*.gguf.part", "*.gguf.part.map"} {
		hits, _ := filepath.Glob(filepath.Join(dir, pat))
		for _, f := range hits {
			if os.Remove(f) == nil {
				removed = append(removed, filepath.Base(f))
			}
		}
	}
	return removed, s.Files().ClearTransfers()
}

func (s *Server) limit() *store.Limit {
	if m := s.Config().PullMbps; m > 0 {
		return store.NewLimit(int64(m) * 125000)
	}
	return nil
}

func (s *Server) filesDir() string {
	if s.files.dir != "" {
		return filepath.Join(s.files.dir, "files")
	}
	return filepath.Join(filepath.Dir(config.StatsPath()), "store", "files")
}

// sourceFor finds a fleet that has the file and a working route to it. from names one; "" tries every machine
// that reports the file complete (not this one).
func (s *Server) sourceFor(ctx context.Context, from, sha string) (store.Source, func(), error) {
	var cands []string
	for _, x := range s.StoreAll(ctx) {
		if x.Store == nil || s.isSelfNode(x.Node) {
			continue
		}
		for _, f := range x.Store.Files {
			if f.SHA256 == sha && f.Complete {
				cands = append(cands, x.Node)
			}
		}
	}
	if from != "" {
		found := false
		for _, c := range cands {
			found = found || strings.EqualFold(c, from)
		}
		if !found {
			return store.Source{}, nil, fmt.Errorf("%s does not have that file (or its fleet is too old to say)", from)
		}
		cands = []string{from}
	}
	if len(cands) == 0 {
		return store.Source{}, nil, fmt.Errorf("no other machine has that file: register it where it is (fleet store add)")
	}
	var lastErr error
	for _, node := range cands {
		for _, r := range s.nodeRoutes(node) {
			base, key, _, rel, skip := s.openRoute(ctx, r, node, nil)
			if skip || base == "" {
				rel()
				continue
			}
			src := store.Source{Base: base, Key: key, HTTP: s.client, Name: node}
			cx, cancel := context.WithTimeout(ctx, 8*time.Second)
			_, err := src.FetchManifest(cx, sha)
			cancel()
			if err == nil {
				return src, rel, nil
			}
			lastErr = fmt.Errorf("%s: %w", node, err)
			rel()
		}
	}
	if lastErr != nil {
		return store.Source{}, nil, lastErr
	}
	return store.Source{}, nil, fmt.Errorf("no route to a fleet that has the file")
}

// claimTransfer takes the one transfer slot of this machine (a transfer reads or writes whole files: one at a
// time keeps the disk and the link usable). It is taken before the request is answered, so a second request
// cannot slip in while the first is still looking for its source.
func (s *Server) claimTransfer(cancel context.CancelFunc) bool {
	f := &s.files
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.transferring || s.Files().Busy() {
		return false
	}
	f.transferring = true
	f.transferCancel = cancel
	return true
}

func (s *Server) releaseTransfer() {
	s.files.mu.Lock()
	s.files.transferring = false
	s.files.transferCancel = nil
	s.files.cacheAt = time.Time{} // a fill changed the worker cache: scan it again at the next readout
	s.files.mu.Unlock()
}

// AbortTransfer cancels any active pull or cache fill on this machine. Returns true if a transfer was aborted.
func (s *Server) AbortTransfer() bool {
	s.files.mu.Lock()
	cancel := s.files.transferCancel
	s.files.mu.Unlock()
	if cancel != nil {
		cancel()
		return true
	}
	return false
}

func (s *Server) bgCtx() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// StartPull begins fetching a file in the background on this machine.
func (s *Server) StartPull(ctx context.Context, req PullRequest) error {
	if len(req.SHA) != 64 {
		return fmt.Errorf("give the full sha256 of the file (fleet store list)")
	}
	pullCtx, cancel := context.WithCancel(s.bgCtx())
	if !s.claimTransfer(cancel) {
		cancel()
		return errBusy
	}
	src, rel, err := s.sourceFor(ctx, req.From, req.SHA)
	if err != nil {
		s.releaseTransfer()
		cancel()
		return err
	}
	dir := req.Dir
	if dir == "" {
		dir = s.filesDir()
	}
	go func() {
		defer s.releaseTransfer()
		defer rel()
		_, err := s.Files().Pull(pullCtx, src, req.SHA, expandUser(dir), store.PullOptions{Limit: s.limit()})
		_ = err // the transfer list says how it went
	}()
	return nil
}

// StartFill begins filling this machine's worker cache in the background.
func (s *Server) StartFill(ctx context.Context, req FillRequest) error {
	if len(req.SHA) != 64 {
		return fmt.Errorf("give the full sha256 of the file (fleet store list)")
	}
	blocks, err := store.ParseBlocks(req.Blocks)
	if err != nil {
		return err
	}
	if len(blocks) == 0 && !req.Output {
		return fmt.Errorf("blocks (like 0-3,7) or output is required")
	}
	if s.ringCacheDir() == "" {
		return fmt.Errorf("this machine has no worker cache directory: set LLAMA_CACHE for the daemon or ring_cache in the configuration")
	}
	fillCtx, cancel := context.WithCancel(s.bgCtx())
	if !s.claimTransfer(cancel) {
		cancel()
		return errBusy
	}
	src, rel, err := s.sourceFor(ctx, req.From, req.SHA)
	if err != nil {
		s.releaseTransfer()
		cancel()
		return err
	}
	dir := s.ringCacheDir()
	go func() {
		defer s.releaseTransfer()
		defer rel()
		s.Files().FillCache(fillCtx, src, req.SHA, dir, store.BlockSet{Blocks: blocks, Output: req.Output}, store.PullOptions{Limit: s.limit()})
	}()
	return nil
}

// forwardStore sends an admin call to the fleet on another machine, over the first route that reaches it.
// Only a route the far side trusts as admin works (an SSH tunnel); anything else is told to run it there.
func (s *Server) forwardStore(ctx context.Context, node, path string, body any) (int, []byte, error) {
	return s.forwardStoreT(ctx, node, path, body, 30*time.Second)
}

func (s *Server) forwardStoreT(ctx context.Context, node, path string, body any, timeout time.Duration) (int, []byte, error) {
	routes := s.nodeRoutes(node)
	if len(routes) == 0 {
		return 0, nil, fmt.Errorf("no route to a fleet on %s", node)
	}
	b, _ := json.Marshal(body)
	var last error
	for _, r := range routes {
		base, key, _, rel, skip := s.openRoute(ctx, r, node, nil)
		if skip || base == "" || r.peer != nil {
			rel()
			continue
		}
		cx, cancel := context.WithTimeout(ctx, timeout)
		req, _ := http.NewRequestWithContext(cx, "POST", base+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			cancel()
			rel()
			last = err
			continue
		}
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		rel()
		if resp.StatusCode == 403 {
			return 403, out, fmt.Errorf("the fleet on %s does not accept commands over this route (%s) — run it on %s itself", node, r.source, node)
		}
		return resp.StatusCode, out, nil
	}
	if last != nil {
		return 0, nil, last
	}
	return 0, nil, fmt.Errorf("no usable route to a fleet on %s (a peer cannot be commanded: run it on %s itself)", node, node)
}

func (s *Server) pullHandlers(mux *http.ServeMux, adm func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /fleet/store/manifest/{sha}", s.v1Auth(func(w http.ResponseWriter, r *http.Request) { s.Files().ManifestHandler()(w, r) }))
	mux.HandleFunc("GET /fleet/store/blob/{sha}", s.v1Auth(func(w http.ResponseWriter, r *http.Request) { s.Files().BlobHandler()(w, r) }))
	command := func(path string, decode func(io.Reader) (node string, run func(context.Context) error, fwd any, err error)) http.HandlerFunc {
		return adm(func(w http.ResponseWriter, r *http.Request) {
			node, run, fwd, err := decode(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				httpErr(w, 400, err.Error())
				return
			}
			if node != "" && !s.isSelfNode(node) {
				code, out, err := s.forwardStore(r.Context(), node, path, fwd)
				if err != nil {
					httpErr(w, 502, err.Error())
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				w.Write(out)
				return
			}
			if err := run(r.Context()); err != nil {
				code := 400
				if err == errBusy {
					code = 409
				}
				httpErr(w, code, err.Error())
				return
			}
			w.WriteHeader(202)
			writeJSON(w, map[string]any{"started": true})
		})
	}
	mux.HandleFunc("POST /fleet/store/pull", command("/fleet/store/pull", func(b io.Reader) (string, func(context.Context) error, any, error) {
		var req PullRequest
		if err := json.NewDecoder(b).Decode(&req); err != nil {
			return "", nil, nil, err
		}
		node := req.Node
		req.Node = ""
		return node, func(ctx context.Context) error { return s.StartPull(ctx, req) }, req, nil
	}))
	mux.HandleFunc("POST /fleet/store/fill", command("/fleet/store/fill", func(b io.Reader) (string, func(context.Context) error, any, error) {
		var req FillRequest
		if err := json.NewDecoder(b).Decode(&req); err != nil {
			return "", nil, nil, err
		}
		node := req.Node
		req.Node = ""
		return node, func(ctx context.Context) error { return s.StartFill(ctx, req) }, req, nil
	}))
	mux.HandleFunc("POST /fleet/store/abort", command("/fleet/store/abort", func(b io.Reader) (string, func(context.Context) error, any, error) {
		var req AbortRequest
		_ = json.NewDecoder(b).Decode(&req)
		node := req.Node
		req.Node = ""
		return node, func(ctx context.Context) error {
			if req.Discard {
				s.DiscardPartials()
				return nil
			}
			if !s.AbortTransfer() {
				return fmt.Errorf("no transfer running on this machine")
			}
			return nil
		}, req, nil
	}))
}
