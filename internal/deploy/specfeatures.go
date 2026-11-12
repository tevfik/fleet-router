package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tevfik/fleet-router/internal/ringengine"
)

// The engine's HELLO feature bits (fleet-engine RING_FEAT_*), as /ring/info worker_features reports them.
const (
	featWireF16   = ringengine.FeatWireF16
	featAsyncHash = ringengine.FeatAsyncHash
	featAsyncCopy = ringengine.FeatAsyncCopy
	featStateSnap = ringengine.FeatStateSnap // recurrent-state checkpoints stay on the worker
	featPeerFwd   = ringengine.FeatPeerFwd   // worker-to-worker forwarding (chain)
	featNoStore   = ringengine.FeatNoStore   // negative bit: worker keeps no weights by hash
	featSession   = ringengine.FeatSession   // SESSION_ATTACH / SESSION_CLOSE
)

// hybridSpecGeneric is what the plan says before the ring is up (or when the engine cannot tell the features).
const hybridSpecGeneric = "speculation on a hybrid (recurrent-layer) model over the ring is fast only if every worker engine has the STATE_SNAP feature (protocol patch 3); an old worker, or GGML_RING_NO_STATE_SNAP=1 (fleet sets GGML_RING_STATE_SNAP=1 on the master for this case), falls back to the wire path which was measured about 6x slower. fleet could not check the worker features (an engine whose /ring/info has no worker_features, or the ring was not reachable): update the workers or use --spec none"

// ringInfoFetcher reads /ring/info from the master's statistics port; tests replace it (no network, no ssh).
var ringInfoFetcher = fetchRingInfo

// parseWorkerFeatures reads worker_features from a /ring/info body. nil means unknown (an engine without the field,
// an empty map because no worker has connected, or an unreadable body): never an error.
func parseWorkerFeatures(body []byte) map[string]int {
	var info struct {
		Features map[string]int `json:"worker_features"`
	}
	if json.Unmarshal(body, &info) != nil || len(info.Features) == 0 {
		return nil
	}
	return info.Features
}

// stateSnapVerdict says which workers lack STATE_SNAP. known is false when the features are unknown.
func stateSnapVerdict(feats map[string]int) (missing []string, known bool) {
	if len(feats) == 0 {
		return nil, false
	}
	for w, f := range feats {
		if f&featStateSnap == 0 {
			missing = append(missing, w)
		}
	}
	sort.Strings(missing)
	return missing, true
}

// checkSpecFeatures runs after the ring is up. For a hybrid model with workers and speculation on, it reads the
// workers' features: a precise warning only when some worker lacks STATE_SNAP, nothing when all have it, and the
// generic warning stays when the engine does not say.
func checkSpecFeatures(ctx context.Context, res *DeployResult, isLocalMaster bool) {
	if res == nil || !res.hybridSpec {
		return
	}
	body, err := ringInfoFetcher(ctx, res.MasterNode, isLocalMaster, res.statsPort)
	if err != nil {
		return
	}
	applySpecFeatures(res, parseWorkerFeatures(body))
}

func applySpecFeatures(res *DeployResult, feats map[string]int) {
	missing, known := stateSnapVerdict(feats)
	if !known {
		return
	}
	var out []string
	for _, w := range res.Warnings {
		if w != hybridSpecGeneric {
			out = append(out, w)
		}
	}
	if len(missing) > 0 {
		out = append(out, fmt.Sprintf("speculation on a hybrid (recurrent-layer) model: worker(s) %s lack the STATE_SNAP feature (old engine, or GGML_RING_NO_STATE_SNAP=1), so the recurrent state travels the wire, measured about 6x slower. Update them or use --spec none", strings.Join(missing, ", ")))
	}
	res.Warnings = out
	res.hybridSpec = false
}
