package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/store"
)

// The model store of this machine: the GGUF files registered here (hashed once), and the worker cache. Every
// fleet reports it with its machine readings (/fleet/host), so any fleet knows who has what; `fleet plan`
// prices transfers from it. See docs/distributed.md, "Model store".

type fileStore struct {
	dir            string // where the store lives (empty: next to the speed statistics); tests set it
	once           sync.Once
	st             *store.Store
	mu             sync.Mutex
	running        bool
	transferring   bool // a pull or a cache fill holds the transfer slot (pull.go)
	cacheAt        time.Time
	cache          *store.CacheInfo
	transferCancel context.CancelFunc
	notes          []string // what the last add found out (Ollama models with a projector, …)
}

const cacheScanEvery = 30 * time.Second

// Files is this machine's model store.
func (s *Server) Files() *store.Store {
	s.files.once.Do(func() {
		dir := s.files.dir
		if dir == "" {
			dir = filepath.Join(filepath.Dir(config.StatsPath()), "store")
		}
		s.files.st = store.Open(dir)
	})
	return s.files.st
}

func (s *Server) ringCacheDir() string {
	if d := s.Config().RingCache; d != "" {
		return d
	}
	return store.CacheDir()
}

// LocalStore is what this machine has: files and cache (the cache directory is scanned at most every 30 s).
func (s *Server) LocalStore() store.Inventory {
	inv := s.Files().Inventory("")
	f := &s.files
	f.mu.Lock()
	inv.Notes = append([]string{}, f.notes...)
	defer f.mu.Unlock()
	if dir := s.ringCacheDir(); dir != "" {
		if f.cache == nil || f.cache.Dir != dir || time.Since(f.cacheAt) > cacheScanEvery {
			f.cache, f.cacheAt = store.ScanCache(dir), time.Now()
		}
		inv.Cache = f.cache
	}
	return inv
}

// ggufIn lists the .gguf files of a path: the file itself, or those in a directory (and one level below).
func ggufIn(p string) []string {
	st, err := os.Stat(p)
	if err != nil {
		return []string{p} // let the add report the error
	}
	if !st.IsDir() {
		return []string{p}
	}
	var out []string
	es, _ := os.ReadDir(p)
	for _, e := range es {
		q := filepath.Join(p, e.Name())
		if e.IsDir() {
			sub, _ := os.ReadDir(q)
			for _, x := range sub {
				if !x.IsDir() && strings.HasSuffix(strings.ToLower(x.Name()), ".gguf") {
					out = append(out, filepath.Join(q, x.Name()))
				}
			}
		} else if strings.HasSuffix(strings.ToLower(e.Name()), ".gguf") {
			out = append(out, q)
		}
	}
	return out
}

// AddFiles registers the files in the background, one after the other (hashing reads the whole file).
// It returns how many it queued; an error when a run is already going.
func (s *Server) AddFiles(paths []string) (int, error) {
	var files []addJob
	var notes []string
	for _, p := range paths {
		p = expandUser(p)
		if store.IsOllamaDir(p) { // Ollama's blobs are GGUFs without the extension: its manifests say which is which
			ms, err := store.ScanOllama(p)
			if err != nil {
				return 0, err
			}
			for _, m := range ms {
				files = append(files, addJob{m.Model, m.FileName(false)})
				if m.Projector != "" {
					files = append(files, addJob{m.Projector, m.FileName(true)})
					notes = append(notes, m.Describe())
				}
			}
			continue
		}
		for _, g := range ggufIn(p) {
			files = append(files, addJob{path: g})
		}
	}
	f := &s.files
	f.mu.Lock()
	if f.running {
		f.mu.Unlock()
		return 0, errBusy
	}
	f.running = true
	f.notes = notes
	f.mu.Unlock()
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		defer func() { f.mu.Lock(); f.running = false; f.mu.Unlock() }()
		for _, j := range files {
			if _, err := s.Files().AddNamed(ctx, j.path, j.name); err != nil && ctx.Err() != nil {
				return
			}
		}
	}()
	return len(files), nil
}

// addJob is one file to register, under the name its Ollama manifest gives it ("" : the GGUF's own).
type addJob struct{ path, name string }

type busyError struct{}

func (busyError) Error() string {
	return "model files are being added already — wait for it to finish"
}

var errBusy error = busyError{}

func expandUser(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, rest)
		}
	}
	return p
}

// NodeStore is one machine's model files, as its fleet reported them.
type NodeStore struct {
	Node   string           `json:"node"`
	Source string           `json:"source"`
	Err    string           `json:"err,omitempty"`
	Store  *store.Inventory `json:"store,omitempty"`
}

// StoreAll asks every machine's fleet what it has.
func (s *Server) StoreAll(ctx context.Context) []NodeStore {
	var out []NodeStore
	for _, ns := range s.Status().Nodes {
		h := s.HostOf(ctx, ns.Node, nil)
		x := NodeStore{Node: ns.Node, Source: h.Source, Err: h.Err, Store: h.Store}
		if x.Err == "" && x.Store == nil {
			x.Err = "its fleet is too old to list model files — run fleet update there"
		}
		out = append(out, x)
	}
	return out
}

func (s *Server) storeHandlers(mux *http.ServeMux, adm func(http.HandlerFunc) http.HandlerFunc) {
	s.pullHandlers(mux, adm)
	s.linkHandlers(mux, adm)
	mux.HandleFunc("GET /fleet/store", adm(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, s.LocalStore()) }))
	mux.HandleFunc("GET /fleet/store/all", adm(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"nodes": s.StoreAll(r.Context())})
	}))
	mux.HandleFunc("POST /fleet/store/add", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Paths []string `json:"paths"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || len(in.Paths) == 0 {
			httpErr(w, 400, "paths: [file or directory, …] is required")
			return
		}
		n, err := s.AddFiles(in.Paths)
		if err != nil {
			httpErr(w, 409, err.Error())
			return
		}
		w.WriteHeader(202)
		writeJSON(w, map[string]any{"queued": n})
	}))
	mux.HandleFunc("POST /fleet/store/rm", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Ref        string `json:"ref"`
			DeleteFile bool   `json:"delete_file"` // also delete the file, when fleet fetched it itself
			Force      bool   `json:"force"`       // although an endpoint of this machine serves a model of that name
			Node       string `json:"node"`        // forget it on that machine's fleet instead (over the ssh routes)
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || in.Ref == "" {
			httpErr(w, 400, "ref (a sha256 prefix or a name) is required")
			return
		}
		if in.Node != "" && !s.isSelfNode(in.Node) {
			fwd := in
			fwd.Node = ""
			code, out, err := s.forwardStore(r.Context(), in.Node, "/fleet/store/rm", fwd)
			if err != nil {
				httpErr(w, 502, err.Error())
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			w.Write(out)
			return
		}
		if in.DeleteFile && !in.Force {
			if users := s.endpointsServingFile(in.Ref); len(users) > 0 {
				httpErr(w, 409, "endpoint(s) "+strings.Join(users, ", ")+" of this machine serve a model of that name: stop them first (fleet endpoint stop), or use --force")
				return
			}
		}
		var e store.Entry
		var err error
		if in.DeleteFile {
			var path string
			e, path, err = s.Files().DeleteFile(in.Ref, s.filesDir())
			if err == nil {
				e.Name += " (deleted " + path + ")"
			}
		} else {
			e, err = s.Files().Remove(in.Ref)
		}
		if err != nil {
			httpErr(w, 404, err.Error())
			return
		}
		writeJSON(w, e)
	}))
	// forget the records whose file is gone or changed (never touches a file)
	mux.HandleFunc("POST /fleet/store/prune", adm(func(w http.ResponseWriter, _ *http.Request) {
		gone, err := s.Files().Prune()
		if err != nil {
			httpErr(w, 500, err.Error())
			return
		}
		writeJSON(w, map[string]any{"forgotten": gone})
	}))
}

// endpointsServingFile names the endpoints of this machine whose model is the registered file ref (by name).
func (s *Server) endpointsServingFile(ref string) []string {
	var name string
	for _, f := range s.Files().Inventory("").Files {
		if strings.HasPrefix(f.SHA256, strings.ToLower(ref)) || f.Name == ref {
			name = f.Name
		}
	}
	norm := func(x string) string { return strings.TrimSuffix(strings.ToLower(filepath.Base(x)), ".gguf") }
	if name == "" {
		return nil
	}
	var out []string
	for _, e := range s.Config().Endpoints {
		if e.Peer == "" && s.isSelfNode(e.Node) && norm(e.Model) == norm(name) {
			out = append(out, e.Name)
		}
	}
	return out
}

// holdings says, for a planned file, what each machine already holds of it.
func (s *Server) holdings(ctx context.Context, m *store.Manifest, nodes []plan.Node) []plan.Node {
	inv := map[string]*store.Inventory{}
	for _, x := range s.StoreAll(ctx) {
		inv[x.Node] = x.Store
	}
	for i := range nodes {
		si := inv[nodes[i].Name]
		if si == nil {
			continue
		}
		for _, f := range si.Files {
			if f.SHA256 == m.SHA256 && f.Complete {
				nodes[i].HasFile = true
			}
		}
		if si.Cache != nil {
			have, _ := m.CacheHit(si.Cache.Files)
			nodes[i].CachedGB = float64(have) / (1 << 30)
		}
	}
	return nodes
}
