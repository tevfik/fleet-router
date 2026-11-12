package plan

// Stage is one device of a fixed split as RoughTokenMS prices it: the blocks it runs and the rate it reads weights at.
type Stage struct {
	Node       string
	Layers     int
	WeightGBps float64 // effective weight read rate while decoding (Node.WeightBW); 0: unknown
	RTTms      float64 // round trip from the master (0: DefaultRTTms); ignored for the first stage (the master)
	LinkGBps   float64 // 0: DefaultLinkGBps
}

// RoughTokenMS is the cost model applied to a split that was not planned (a fixed -ts): the first stage is the
// master and holds the output projection; every other stage adds its round trip and the hidden state over its link
// both ways. The blocks are read in stage order. ok is false when a stage's rate is unknown: there is no estimate
// without one (fleet does not make one up).
func RoughTokenMS(m *Model, stages []Stage, o Options) (ms float64, ok bool) {
	if m == nil || len(stages) == 0 || len(m.LayerRead) == 0 {
		return 0, false
	}
	block := 0
	ms = orDefault(o.FixedMS, DefaultFixedMS)
	for i, s := range stages {
		if s.WeightGBps <= 0 {
			return 0, false
		}
		var read int64
		for l := 0; l < s.Layers && block < len(m.LayerRead); l++ {
			read += m.LayerRead[block]
			block++
		}
		if i == 0 {
			read += m.OutputBytes
		}
		ms += float64(read) / gib / s.WeightGBps * 1000
		if i > 0 {
			ms += orDefault(s.RTTms, DefaultRTTms) + 2*float64(o.boundary(m))/(orDefault(s.LinkGBps, DefaultLinkGBps)*1e9)*1000
		}
	}
	return ms, true
}
