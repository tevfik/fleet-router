package board

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/store"
)

// StoreInfo describes one model of the inventories from what its GGUF header says (nothing is taken from the name):
// architecture, blocks, experts, trained context, quantization, what the header implies it can do, and who has it.
func StoreInfo(nodes []server.NodeStore, ref string) ([]string, error) {
	rows, err := ResolveModel(nodes, ref)
	if len(rows) == 0 {
		return nil, err
	}
	m := StoreModels(rows)[0]
	var first store.Entry
	var have []string
	for _, n := range nodes {
		if n.Store == nil {
			continue
		}
		for _, f := range n.Store.Files {
			if f.SHA256 == rows[0].SHA256 {
				first = f
			}
		}
		if hasAllShards(m, n.Node) {
			have = append(have, n.Node)
		}
	}
	title := m.Name
	if m.Total > 1 {
		title = fmt.Sprintf("%s (%d files, %s)", m.Name, m.Total, Bytes(m.Size))
	} else {
		title = fmt.Sprintf("%s (%s)", m.Name, Bytes(m.Size))
	}
	l := []string{title}
	if m.Total == 1 {
		l = append(l, "  sha256   "+first.SHA256)
	} else {
		var shas []string
		for _, s := range m.Shards {
			shas = append(shas, s.SHA256[:min(8, len(s.SHA256))])
		}
		l = append(l, "  sha256   "+strings.Join(shas, " "))
	}
	if first.Arch == "" {
		l = append(l, "  header   not read yet (a file added by an older fleet: fleet store add it again)")
	} else {
		kind := "model"
		if first.Kind == "mmproj" {
			kind = "vision projector (mmproj): only works beside the model it was made for"
		}
		l = append(l, fmt.Sprintf("  kind     %s", kind))
		l = append(l, fmt.Sprintf("  arch     %s · %d blocks%s", first.Arch, first.Layers, experts(first)))
		if first.Quant != "" || first.Ctx > 0 {
			l = append(l, fmt.Sprintf("  quant    %s · trained for %d tokens of context", orQ(first.Quant), first.Ctx))
		}
		if len(first.Caps) > 0 {
			l = append(l, "  header says it can: "+strings.Join(first.Caps, ", ")+"   (from the chat template and pooling; fleet probe <endpoint> shows what it really does once it runs)")
		}
	}
	sort.Strings(have)
	if len(have) == 0 {
		have = []string{"no machine has every file"}
	}
	l = append(l, "  on       "+strings.Join(have, ", "))
	return l, nil
}

func hasAllShards(m ModelRow, node string) bool {
	return m.Count[node] == len(m.Shards) && len(m.Shards) >= max(m.Total, 1)
}

func experts(e store.Entry) string {
	if e.Experts > 0 {
		return fmt.Sprintf(" · %d experts", e.Experts)
	}
	return ""
}

func orQ(s string) string {
	if s == "" {
		return "?"
	}
	return s
}
