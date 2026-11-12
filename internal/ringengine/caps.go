package ringengine

import (
	_ "embed"
	"encoding/json"
	"sort"
	"sync"
)

//go:embed engine-caps.json
var embeddedCaps []byte

// The engine's HELLO feature bits (fleet-engine RING_FEAT_*), defined in include/engine-caps.json.
const (
	FeatWireF16   = 1 << 0 // F16 / Q8_0 activations on the wire
	FeatAsyncHash = 1 << 1 // SET_TENSOR_HASH_ASYNC
	FeatAsyncCopy = 1 << 2 // COPY_TENSOR_ASYNC
	FeatStateSnap = 1 << 3 // recurrent-state checkpoints stay on the worker
	FeatPeerFwd   = 1 << 4 // worker-to-worker forwarding (chain)
	FeatNoStore   = 1 << 5 // negative bit: worker keeps no weights by hash
	FeatSession   = 1 << 6 // SESSION_ATTACH / SESSION_CLOSE: lost connection resumes with replay

	KnownFeatureMask = FeatWireF16 | FeatAsyncHash | FeatAsyncCopy | FeatStateSnap | FeatPeerFwd | FeatNoStore | FeatSession
)

type RingProtocolVersion struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
	Patch int `json:"patch"`
}

type StageProtocolVersion struct {
	Magic   string `json:"magic"`
	Version int    `json:"version"`
}

type FeatureSpec struct {
	Bit         int    `json:"bit"`
	Mask        int    `json:"mask"`
	CName       string `json:"c_name"`
	GoName      string `json:"go_name"`
	Cap         string `json:"cap"`
	SincePatch  int    `json:"since_patch"`
	Description string `json:"description"`
}

type EngineCaps struct {
	Schema        string               `json:"schema"`
	RingProtocol  RingProtocolVersion  `json:"ring_protocol"`
	StageProtocol StageProtocolVersion `json:"stage_protocol"`
	RingFeatures  []FeatureSpec        `json:"ring_features"`
}

var (
	capsOnce sync.Once
	capsVal  *EngineCaps
	capsErr  error
)

// LoadCaps parses the embedded engine-caps.json contract.
func LoadCaps() (*EngineCaps, error) {
	capsOnce.Do(func() {
		var c EngineCaps
		if err := json.Unmarshal(embeddedCaps, &c); err != nil {
			capsErr = err
			return
		}
		capsVal = &c
	})
	return capsVal, capsErr
}

// HasFeature reports whether mask has the given feature bit(s) set.
func HasFeature(mask, feat int) bool {
	return mask&feat == feat
}

// FeatureNames decodes a HELLO feature bitmask into known capability names (e.g. "wire.f16", "session.resume")
// and any unknown bit indices (0..30) that are not in the embedded engine-caps.json contract.
func FeatureNames(mask int) (known []string, unknownBits []int) {
	c, err := LoadCaps()
	byBit := map[int]string{}
	if err == nil && c != nil {
		for _, f := range c.RingFeatures {
			byBit[f.Bit] = f.Cap
		}
	}
	for b := 0; b < 31; b++ {
		if mask&(1<<b) == 0 {
			continue
		}
		if capName, ok := byBit[b]; ok {
			known = append(known, capName)
		} else {
			unknownBits = append(unknownBits, b)
		}
	}
	sort.Strings(known)
	return known, unknownBits
}
