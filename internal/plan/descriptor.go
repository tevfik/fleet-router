package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DescriptorFile is the name of the model descriptor inside a model directory.
const DescriptorFile = "fleet-model.json"

// Runtime kinds a descriptor can name.
const (
	RuntimeRing      = "ring"       // llama-ring over a GGUF (tensor split, the ring)
	RuntimeStagePipe = "stage-pipe" // a chain of stage servers, each holding a contiguous range of layers
)

// State kinds of a layer.
const (
	StateAttentionKV = "attention-kv" // a KV cache that grows with every token
	StateRecurrent   = "recurrent"    // a fixed-size state (linear attention)
	StateNone        = "none"
)

// Descriptor says what a model that is not one GGUF file is made of: a directory of compiled per-layer files
// whose sizes the planner cannot read from a header. The model's maker (or a script that measures the
// directory) writes it next to the files as fleet-model.json.
//
// Differences from the first sketch of this format, and why:
//   - no "$schema" URL (it named a site that does not exist); the version is the integer Version.
//   - the sketch listed two example layers under a count of 64: here every layer must be listed, and the
//     count must equal the list (the planner prices layer by layer).
//   - "state_bytes" meant two things (a fixed state, or growth per token): split into KVBytesPerToken and
//     FixedStateBytes.
//   - "hidden_bytes" was a dtype-dependent number: HiddenSize (values) and BoundaryValueBytes (the width the
//     runtime sends them in) are kept apart; the planner multiplies them.
//   - "npu_pool_bytes" and "file_bytes" are named ResidentBytes and Bytes: the first is what a layer takes
//     where it runs (the capacity limit), the second what is on disk (the transfer size).
type Descriptor struct {
	Version      int    `json:"version"` // 1
	Name         string `json:"name"`
	Architecture string `json:"architecture"`
	Runtime      string `json:"runtime_kind"` // RuntimeRing | RuntimeStagePipe

	LayerCount int `json:"layer_count"`
	HiddenSize int `json:"hidden_size"` // values in the state that crosses a stage boundary
	// BoundaryValueBytes is the width of one such value on the wire (0: 2, a 16-bit value [derived] from the
	// 10 KB per token of the 3-board report: 5120 values × 2 bytes).
	BoundaryValueBytes int `json:"boundary_value_bytes,omitempty"`
	VocabSize          int `json:"vocab_size"`
	ContextMax         int `json:"context_max,omitempty"`

	Experts       int `json:"experts,omitempty"`        // 0: dense
	ExpertsActive int `json:"experts_active,omitempty"` // experts one token reads

	// Where the token embedding table and the output head sit: "first" / "last" stage. The planner places the
	// embedding on the machine that takes requests and the head on the last stage, so other values are refused.
	EmbeddingAt    string `json:"embedding_at"`
	EmbeddingBytes int64  `json:"embedding_bytes"`
	HeadAt         string `json:"head_at"`
	HeadBytes      int64  `json:"head_bytes"`

	Layers []DescriptorLayer `json:"layers"`
}

// DescriptorLayer is one layer.
type DescriptorLayer struct {
	Index         int    `json:"index"`
	File          string `json:"file,omitempty"`
	Bytes         int64  `json:"bytes"`                    // on disk
	ResidentBytes int64  `json:"resident_bytes,omitempty"` // where it runs (0: Bytes)
	// ReadBytes is what one token reads from it (0: Bytes); a mixture-of-experts layer reads less.
	ReadBytes       int64  `json:"read_bytes_per_token,omitempty"`
	State           string `json:"state"`                        // StateAttentionKV | StateRecurrent | StateNone
	KVBytesPerToken int64  `json:"kv_bytes_per_token,omitempty"` // attention-kv only
	FixedStateBytes int64  `json:"fixed_state_bytes,omitempty"`  // recurrent only
}

// ParseDescriptor reads a descriptor and checks it strictly: unknown fields, inconsistent counts and
// impossible numbers are errors.
func ParseDescriptor(r io.Reader) (*Descriptor, error) {
	dec := json.NewDecoder(io.LimitReader(r, 64<<20))
	dec.DisallowUnknownFields()
	var d Descriptor
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("%s: %w", DescriptorFile, err)
	}
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", DescriptorFile, err)
	}
	return &d, nil
}

// Validate says what is wrong with a descriptor.
func (d *Descriptor) Validate() error {
	if d.Version != 1 {
		return fmt.Errorf("version %d is not supported (1)", d.Version)
	}
	if strings.TrimSpace(d.Name) == "" || strings.TrimSpace(d.Architecture) == "" {
		return fmt.Errorf("name and architecture are required")
	}
	switch d.Runtime {
	case RuntimeRing, RuntimeStagePipe:
	default:
		return fmt.Errorf("runtime_kind %q is not %q or %q", d.Runtime, RuntimeRing, RuntimeStagePipe)
	}
	if d.LayerCount <= 0 {
		return fmt.Errorf("layer_count must be positive")
	}
	if len(d.Layers) != d.LayerCount {
		return fmt.Errorf("layer_count is %d but %d layers are listed", d.LayerCount, len(d.Layers))
	}
	if d.HiddenSize <= 0 || d.VocabSize <= 0 {
		return fmt.Errorf("hidden_size and vocab_size must be positive")
	}
	if d.BoundaryValueBytes < 0 {
		return fmt.Errorf("boundary_value_bytes is negative")
	}
	if d.ContextMax < 0 {
		return fmt.Errorf("context_max is negative")
	}
	if d.Experts < 0 || d.ExpertsActive < 0 || d.ExpertsActive > d.Experts {
		return fmt.Errorf("experts_active (%d) must be within experts (%d)", d.ExpertsActive, d.Experts)
	}
	if d.EmbeddingAt != "first" {
		return fmt.Errorf("embedding_at %q: only \"first\" is supported", d.EmbeddingAt)
	}
	if d.HeadAt != "last" {
		return fmt.Errorf("head_at %q: only \"last\" is supported", d.HeadAt)
	}
	if d.EmbeddingBytes < 0 || d.HeadBytes < 0 {
		return fmt.Errorf("embedding_bytes and head_bytes must not be negative")
	}
	for i, l := range d.Layers {
		if l.Index != i {
			return fmt.Errorf("layers[%d] has index %d: the layers must be listed in order from 0", i, l.Index)
		}
		if l.Bytes <= 0 {
			return fmt.Errorf("layer %d: bytes must be positive", i)
		}
		if l.ResidentBytes < 0 || l.ReadBytes < 0 || l.KVBytesPerToken < 0 || l.FixedStateBytes < 0 {
			return fmt.Errorf("layer %d: a size is negative", i)
		}
		if l.ReadBytes > l.Bytes {
			return fmt.Errorf("layer %d: read_bytes_per_token (%d) exceeds bytes (%d)", i, l.ReadBytes, l.Bytes)
		}
		switch l.State {
		case StateAttentionKV:
			if l.KVBytesPerToken <= 0 || l.FixedStateBytes != 0 {
				return fmt.Errorf("layer %d: an attention-kv layer needs kv_bytes_per_token and no fixed_state_bytes", i)
			}
		case StateRecurrent:
			if l.FixedStateBytes <= 0 || l.KVBytesPerToken != 0 {
				return fmt.Errorf("layer %d: a recurrent layer needs fixed_state_bytes and no kv_bytes_per_token", i)
			}
		case StateNone:
			if l.KVBytesPerToken != 0 || l.FixedStateBytes != 0 {
				return fmt.Errorf("layer %d: a stateless layer carries no state sizes", i)
			}
		default:
			return fmt.Errorf("layer %d: state %q is not attention-kv, recurrent or none", i, l.State)
		}
	}
	return nil
}

// Model turns the descriptor into what the planner prices. The planner has one size per block, so the
// capacity figure is ResidentBytes where given. The fixed state of recurrent layers is not priced (it does
// not grow with the context and the planner has no field for it); on a pool that is full to the last byte
// this is a known gap.
func (d *Descriptor) Model() (*Model, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	vb := d.BoundaryValueBytes
	if vb == 0 {
		vb = 2
	}
	m := &Model{Name: d.Name, Arch: d.Architecture, Layers: d.LayerCount, Embd: d.HiddenSize, CtxMax: d.ContextMax,
		Experts: d.Experts, ExpertsUsed: d.ExpertsActive, Vocab: d.VocabSize, Runtime: d.Runtime,
		HiddenBytes: int64(d.HiddenSize) * int64(vb), EmbdBytes: d.EmbeddingBytes, OutputBytes: d.HeadBytes,
		OtherBytes: d.EmbeddingBytes + d.HeadBytes}
	m.FileBytes = m.OtherBytes
	var kv int64
	for _, l := range d.Layers {
		res, read := l.ResidentBytes, l.ReadBytes
		if res == 0 {
			res = l.Bytes
		}
		if read == 0 {
			read = l.Bytes
		}
		m.LayerBytes = append(m.LayerBytes, res)
		m.LayerRead = append(m.LayerRead, read)
		m.FileBytes += l.Bytes
		kv += l.KVBytesPerToken
	}
	m.KVPerTokenLayer = kv / int64(d.LayerCount) // the layers that keep no KV cache bring the average down
	return m, nil
}

// ReadDescriptor reads a fleet-model.json (path to the file, or to the directory that holds it).
func ReadDescriptor(path string) (*Model, error) {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		path = filepath.Join(path, DescriptorFile)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	d, err := ParseDescriptor(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return d.Model()
}

// IsDescriptorPath says whether a model argument names a descriptor: a .json file or a directory.
func IsDescriptorPath(path string) bool {
	if strings.EqualFold(filepath.Ext(path), ".json") {
		return true
	}
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// LoadModel reads a model given as a GGUF file (or the first shard of one), or as a descriptor.
func LoadModel(path string) (*Model, error) {
	if IsDescriptorPath(path) {
		return ReadDescriptor(path)
	}
	return ReadGGUF(path)
}
