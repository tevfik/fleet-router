package ringengine

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestLoadCapsAndConstants(t *testing.T) {
	c, err := LoadCaps()
	if err != nil {
		t.Fatalf("LoadCaps failed: %v", err)
	}
	if c.Schema != "fleet-engine-caps/v1" {
		t.Fatalf("unexpected schema %q", c.Schema)
	}
	if c.RingProtocol.Major != 101 || c.RingProtocol.Minor != 0 || c.RingProtocol.Patch < 6 {
		t.Fatalf("unexpected ring_protocol: %+v", c.RingProtocol)
	}
	if c.StageProtocol.Magic != "FSP1" || c.StageProtocol.Version != 1 {
		t.Fatalf("unexpected stage_protocol: %+v", c.StageProtocol)
	}

	wantGo := map[string]int{
		"FeatWireF16":   FeatWireF16,
		"FeatAsyncHash": FeatAsyncHash,
		"FeatAsyncCopy": FeatAsyncCopy,
		"FeatStateSnap": FeatStateSnap,
		"FeatPeerFwd":   FeatPeerFwd,
		"FeatNoStore":   FeatNoStore,
		"FeatSession":   FeatSession,
	}
	if len(c.RingFeatures) != len(wantGo) {
		t.Fatalf("expected %d ring_features, got %d", len(wantGo), len(c.RingFeatures))
	}
	maskUnion := 0
	for _, f := range c.RingFeatures {
		if f.Mask != (1 << f.Bit) {
			t.Errorf("feature %s: mask %d != 1<<%d", f.CName, f.Mask, f.Bit)
		}
		goVal, ok := wantGo[f.GoName]
		if !ok {
			t.Errorf("unexpected go_name %q in engine-caps.json", f.GoName)
			continue
		}
		if goVal != f.Mask {
			t.Errorf("%s = %d, want %d", f.GoName, goVal, f.Mask)
		}
		maskUnion |= f.Mask
	}
	if maskUnion != KnownFeatureMask {
		t.Errorf("KnownFeatureMask = 0x%x, union of JSON features = 0x%x", KnownFeatureMask, maskUnion)
	}
}

func TestFeatureNamesAndUnknownBits(t *testing.T) {
	mask := FeatWireF16 | FeatStateSnap | FeatSession | (1 << 9)
	if !HasFeature(mask, FeatSession) {
		t.Errorf("expected HasFeature(mask, FeatSession) == true")
	}
	if HasFeature(mask, FeatPeerFwd) {
		t.Errorf("expected HasFeature(mask, FeatPeerFwd) == false")
	}
	known, unknown := FeatureNames(mask)
	wantKnown := "session.resume,state.snap,wire.f16"
	if got := strings.Join(known, ","); got != wantKnown {
		t.Errorf("known = %q, want %q", got, wantKnown)
	}
	if len(unknown) != 1 || unknown[0] != 9 {
		t.Errorf("unknown = %v, want [9]", unknown)
	}
}

func findFleetEngineRoot() string {
	for _, cand := range []string{os.Getenv("FLEET_ENGINE_DIR"), "../../../fleet-engine", "../../../../fleet-engine"} {
		if cand == "" {
			continue
		}
		p := filepath.Clean(cand)
		if _, err := os.Stat(filepath.Join(p, "include", "ggml-ring.h")); err == nil {
			return p
		}
	}
	return ""
}

func TestEngineCapsMatchesFleetEngineSource(t *testing.T) {
	engineRoot := findFleetEngineRoot()
	if engineRoot == "" {
		t.Skip("sibling fleet-engine not present; skipping cross-repo sync check")
	}
	capsPath := filepath.Join(engineRoot, "include", "engine-caps.json")
	extBytes, err := os.ReadFile(capsPath)
	if os.IsNotExist(err) {
		t.Skip("sibling fleet-engine/include/engine-caps.json not present; skipping cross-repo sync check")
	}
	if err != nil {
		t.Fatalf("read %s: %v", capsPath, err)
	}
	if strings.TrimSpace(string(extBytes)) != strings.TrimSpace(string(embeddedCaps)) {
		t.Fatalf("internal/ringengine/engine-caps.json is out of sync with ../fleet-engine/include/engine-caps.json (run `make sync-engine-caps`)")
	}

	hdrPath := filepath.Join(engineRoot, "include", "ggml-ring.h")
	hdrBytes, err := os.ReadFile(hdrPath)
	if err != nil {
		t.Fatalf("read %s: %v", hdrPath, err)
	}
	hdr := string(hdrBytes)
	c, err := LoadCaps()
	if err != nil {
		t.Fatal(err)
	}

	rePatch := regexp.MustCompile(`#define\s+RPC_PROTO_PATCH_VERSION\s+(\d+)`)
	if m := rePatch.FindStringSubmatch(hdr); m != nil {
		patch, _ := strconv.Atoi(m[1])
		if patch != c.RingProtocol.Patch {
			t.Errorf("ggml-ring.h RPC_PROTO_PATCH_VERSION = %d, engine-caps.json = %d", patch, c.RingProtocol.Patch)
		}
	} else {
		t.Errorf("RPC_PROTO_PATCH_VERSION not found in ggml-ring.h")
	}

	cppPath := filepath.Join(engineRoot, "src", "ggml-ring", "ggml-ring.cpp")
	cppBytes, err := os.ReadFile(cppPath)
	if err != nil {
		t.Fatalf("read %s: %v", cppPath, err)
	}
	reFeat := regexp.MustCompile(`(RING_FEAT_[A-Z0-9_]+)\s*=\s*1\s*<<\s*(\d+)`)
	matches := reFeat.FindAllStringSubmatch(string(cppBytes), -1)
	if len(matches) == 0 {
		t.Fatalf("no RING_FEAT_* enum entries found in ggml-ring.cpp")
	}
	inHdr := map[string]int{}
	for _, m := range matches {
		bit, _ := strconv.Atoi(m[2])
		inHdr[m[1]] = bit
	}
	if len(inHdr) != len(c.RingFeatures) {
		t.Errorf("ggml-ring.cpp has %d RING_FEAT_* entries, engine-caps.json has %d", len(inHdr), len(c.RingFeatures))
	}
	for _, f := range c.RingFeatures {
		bit, ok := inHdr[f.CName]
		if !ok {
			t.Errorf("engine-caps.json feature %s missing from ggml-ring.cpp", f.CName)
		} else if bit != f.Bit {
			t.Errorf("feature %s bit = %d in JSON, %d in ggml-ring.cpp", f.CName, f.Bit, bit)
		}
	}
}
