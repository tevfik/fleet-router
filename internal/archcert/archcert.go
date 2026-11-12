// Package archcert answers "did this model architecture pass the ring's correctness gate?". The record is arch-cert.json
// of fleet-engine (scripts/arch-cert.sh writes it); a copy is embedded so fleet works without the engine's sources, and
// FLEET_ARCH_CERT names a newer file.
package archcert

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

//go:embed arch-cert.json
var embedded []byte

// Status of one architecture.
const (
	Certified   = "certified"   // the exact (f32) wire passed the gate on a GPU worker
	Failed      = "failed"      // it did not
	Uncertified = "uncertified" // never run
)

type Run struct {
	File     string  `json:"file"`
	Wire     string  `json:"wire"`
	Topology string  `json:"topology"`
	KLD      float64 `json:"kld"`
	Top1Pct  float64 `json:"top1_pct"`
	Gate     string  `json:"gate"`
	Info     bool    `json:"informational"`
}

type Entry struct {
	Kind        string `json:"kind"` // dense | moe
	Note        string `json:"note,omitempty"`
	Status      string `json:"status"`
	LossyWireOK *bool  `json:"lossy_wire_ok"` // nil: never tried
	// RequiresEnv: the gate passed only with these engine environment variables set on every machine (KEY=VALUE,
	// space separated), e.g. GGML_CUDA_DISABLE_FUSION=1 for qwen4exp (CUDA kernel fusion partitions the graph differently
	// on the ring than on one device and the last-bit differences add up over the layers).
	RequiresEnv string `json:"requires_env,omitempty"`
	Runs        []Run  `json:"runs"`
}

type Cert struct {
	Archs map[string]Entry `json:"archs"`
}

// Load reads FLEET_ARCH_CERT when set, the embedded list otherwise.
func Load() (*Cert, error) {
	b := embedded
	if p := os.Getenv("FLEET_ARCH_CERT"); p != "" {
		var err error
		if b, err = os.ReadFile(p); err != nil {
			return nil, err
		}
	}
	return parse(b)
}

func parse(b []byte) (*Cert, error) {
	var c Cert
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Status of an architecture (GGUF general.architecture); an empty one is uncertified.
func (c *Cert) Status(arch string) (string, Entry) {
	e, ok := c.Archs[arch]
	if !ok || arch == "" {
		return Uncertified, Entry{}
	}
	return e.Status, e
}

// Warn is the sentence to show before a model is split over machines with this wire ("" when nothing is wrong).
// wire is "", "f32", "f16" or "fp8".
func Warn(arch, wire string) string {
	c, err := Load()
	if err != nil {
		return ""
	}
	return c.Warn(arch, wire)
}

func (c *Cert) Warn(arch, wire string) string {
	st, e := c.Status(arch)
	wire = strings.ToLower(wire)
	switch st {
	case Uncertified:
		if arch == "" {
			return "architecture unknown: the ring's correctness gate was not checked for this model (scripts/arch-cert.sh)"
		}
		return fmt.Sprintf("architecture %q has not passed the ring correctness gate: run scripts/arch-cert.sh before trusting a split", arch)
	case Failed:
		return fmt.Sprintf("architecture %q FAILED the ring correctness gate (see arch-cert.json): a split may change the output", arch)
	}
	if wire == "f16" || wire == "fp8" {
		switch {
		case e.LossyWireOK == nil:
			return fmt.Sprintf("the %s wire was never gated on architecture %q: it can change the output (f32 is exact)", wire, arch)
		case !*e.LossyWireOK:
			return fmt.Sprintf("the %s wire FAILED the gate on architecture %q: use the f32 wire", wire, arch)
		}
	}
	return ""
}

// RequiredEnv is the KEY=VALUE list the architecture's gate result depends on (nil: none). fleet sets it on the master
// and on every worker of a ring.
func RequiredEnv(arch string) []string {
	c, err := Load()
	if err != nil {
		return nil
	}
	return c.RequiredEnv(arch)
}

func (c *Cert) RequiredEnv(arch string) []string {
	st, e := c.Status(arch)
	if st != Certified {
		return nil
	}
	var out []string
	for _, kv := range strings.Fields(e.RequiresEnv) {
		if strings.Contains(kv, "=") {
			out = append(out, kv)
		}
	}
	return out
}
