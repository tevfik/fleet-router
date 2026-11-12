package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// APIKey represents an authorized client token with QoS profile, model access controls, and usage metrics.
type APIKey struct {
	Key            string    `json:"key"`
	Name           string    `json:"name"`
	QoS            string    `json:"qos"` // ultra-low-latency | max-throughput | memory-saver | default
	Role           string    `json:"role,omitempty"`
	AllowedModels  []string  `json:"allowed_models,omitempty"`
	AllowedRoles   []string  `json:"allowed_roles,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	LastUsedAt     time.Time `json:"last_used_at"`
	TotalRequests  int64     `json:"total_requests"`
	TotalTokens    int64     `json:"total_tokens"`
	TotalLatencyMS int64     `json:"total_latency_ms"`
}

// AllowsModel checks whether this key is permitted to query the given model name.
// If AllowedModels is empty or contains "*", any model is permitted.
// Supports glob patterns (e.g. "qwen*", "*deepseek*").
func (ak *APIKey) AllowsModel(model string) bool {
	if len(ak.AllowedModels) == 0 {
		return true
	}
	mTarget := strings.ToLower(strings.TrimSpace(model))
	for _, m := range ak.AllowedModels {
		pat := strings.ToLower(strings.TrimSpace(m))
		if pat == "*" || pat == mTarget {
			return true
		}
		if matched, _ := filepath.Match(pat, mTarget); matched {
			return true
		}
	}
	return false
}

// AllowsRole checks whether this key possesses the required role (e.g. "admin", "inference").
// If the key has no role restrictions, or Role is "admin", all roles are permitted.
func (ak *APIKey) AllowsRole(role string) bool {
	if role == "" || (ak.Role == "" && len(ak.AllowedRoles) == 0) {
		return true
	}
	if strings.EqualFold(ak.Role, "admin") {
		return true
	}
	if strings.EqualFold(ak.Role, role) {
		return true
	}
	for _, r := range ak.AllowedRoles {
		if strings.EqualFold(r, "admin") || strings.EqualFold(r, role) {
			return true
		}
	}
	return false
}

// KeyStore manages persistent API keys and client tracking.
type KeyStore struct {
	mu   sync.RWMutex
	path string
	keys map[string]*APIKey
}

var (
	globalKeyStore     *KeyStore
	globalKeyStoreOnce sync.Once
)

// GetKeyStore returns the singleton KeyStore instance.
func GetKeyStore() *KeyStore {
	globalKeyStoreOnce.Do(func() {
		home, _ := os.UserHomeDir()
		storePath := filepath.Join(home, ".cache", "fleet", "apikeys.json")
		globalKeyStore = newKeyStore(storePath)
	})
	return globalKeyStore
}

// SetKeyStore overrides the global key store (useful for testing or custom paths).
func SetKeyStore(ks *KeyStore) {
	globalKeyStoreOnce.Do(func() {})
	globalKeyStore = ks
}

func newKeyStore(path string) *KeyStore {
	ks := &KeyStore{
		path: path,
		keys: make(map[string]*APIKey),
	}
	ks.load()
	return ks
}

func (ks *KeyStore) load() {
	ks.mu.Lock()
	defer ks.mu.Unlock()

	data, err := os.ReadFile(ks.path)
	if err != nil {
		return
	}
	var list []*APIKey
	if err := json.Unmarshal(data, &list); err == nil {
		for _, k := range list {
			ks.keys[k.Key] = k
		}
	}
}

func (ks *KeyStore) save() error {
	_ = os.MkdirAll(filepath.Dir(ks.path), 0700)
	list := make([]*APIKey, 0, len(ks.keys))
	for _, k := range ks.keys {
		list = append(list, k)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	// atomic and durable: a crash in the middle of a plain WriteFile left an empty file, i.e. no keys at all
	f, err := os.CreateTemp(filepath.Dir(ks.path), filepath.Base(ks.path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0600)
	}
	if werr == nil {
		werr = os.Rename(tmp, ks.path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}

// GenerateKey produces a random secure token with flt_live_ prefix.
func GenerateKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "flt_live_" + hex.EncodeToString(b)
}

// Create generates and stores a new API Key with a specific QoS profile and optional allowed models.
func (ks *KeyStore) Create(name, qos, role string, models ...string) (*APIKey, error) {
	ks.mu.Lock()
	defer ks.mu.Unlock()

	if qos == "" {
		qos = "default"
	}

	var allowedModels []string
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m != "" {
			allowedModels = append(allowedModels, m)
		}
	}

	key := GenerateKey()
	ak := &APIKey{
		Key:           key,
		Name:          name,
		QoS:           qos,
		Role:          role,
		AllowedModels: allowedModels,
		CreatedAt:     time.Now(),
		LastUsedAt:    time.Now(),
	}
	ks.keys[key] = ak
	_ = ks.save()
	return ak, nil
}

// Lookup finds an APIKey without incrementing request counters.
func (ks *KeyStore) Lookup(token string) (*APIKey, bool) {
	ks.mu.RLock()
	defer ks.mu.RUnlock()

	ak, ok := ks.keys[token]
	if !ok {
		return nil, false
	}
	cp := *ak // a copy: Validate mutates the stored one under the lock
	return &cp, true
}

// Validate checks if a token is valid and updates its last used timestamp and request count.
func (ks *KeyStore) Validate(token string) (*APIKey, bool) {
	ks.mu.Lock()
	defer ks.mu.Unlock()

	ak, ok := ks.keys[token]
	if !ok {
		return nil, false
	}
	ak.LastUsedAt = time.Now()
	ak.TotalRequests++
	cp := *ak
	return &cp, true
}

// RecordUsage increments token and latency counters for a key.
func (ks *KeyStore) RecordUsage(token string, tokens int, latencyMS int64) {
	ks.mu.Lock()
	defer ks.mu.Unlock()

	if ak, ok := ks.keys[token]; ok {
		ak.TotalTokens += int64(tokens)
		ak.TotalLatencyMS += latencyMS
		_ = ks.save()
	}
}

// List returns all registered API keys.
func (ks *KeyStore) List() []*APIKey {
	ks.mu.RLock()
	defer ks.mu.RUnlock()

	out := make([]*APIKey, 0, len(ks.keys))
	for _, k := range ks.keys {
		cp := *k
		out = append(out, &cp)
	}
	return out
}

// Delete removes an API key.
func (ks *KeyStore) Delete(token string) error {
	ks.mu.Lock()
	defer ks.mu.Unlock()

	delete(ks.keys, token)
	return ks.save()
}

// ExtractBearer extracts token from Authorization: Bearer <token>
func ExtractBearer(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	return ""
}
