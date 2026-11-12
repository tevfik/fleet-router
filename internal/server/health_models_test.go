package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

// One Ollama serves every local model: a health round must ask its /models once, not once per endpoint
// (found on a laptop: 9 endpoints on 127.0.0.1:11434 → 180k /v1/models a day).
func TestHealth_ModelsSharedPerRound(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"}]}`))
	}))
	defer srv.Close()
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{
		rtEp("ea", "n1", srv.URL+"/v1", "a"), rtEp("eb", "n1", srv.URL+"/v1", "b"), rtEp("ec", "n1", srv.URL+"/v1", "c"),
	}})
	rctx := withModelsRound(context.Background())
	for _, e := range s.Config().Endpoints {
		s.probe(rctx, &e)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("/models asked %d times in one round, want 1", n)
	}
	if !s.getState("ea").OK || !s.getState("eb").OK {
		t.Fatalf("listed models must be healthy: %+v %+v", s.getState("ea"), s.getState("eb"))
	}
	if st := s.getState("ec"); st.OK || st.Err != "model not listed" {
		t.Fatalf("each endpoint still matches its own model: %+v", st)
	}
	// a new round asks again
	rctx = withModelsRound(context.Background())
	ep := s.Config().Endpoints[0]
	s.probe(rctx, &ep)
	if n := hits.Load(); n != 2 {
		t.Fatalf("next round: %d hits, want 2", n)
	}
}

// A server nobody reaches: the group's engine calls (/health, /props, /slots, /metrics) must not dial it again.
func TestHealth_UnreachableGroupSkipsEngineCalls(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // refused from now on
	e := rtEp("g", "n1", url+"/v1", "m")
	e.Probe = "any"
	e.Group = &config.Group{Engine: "llama-ring"}
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{e}})
	s.hosts.m = map[string]HostReport{}
	ep := s.Config().Endpoints[0]
	s.probe(context.Background(), &ep)
	st := s.getState("g")
	if st.OK || !st.noConn {
		t.Fatalf("a refused dial is a connection failure: %+v", st)
	}
	if st.Group == nil || st.Group.Health != "" || st.Group.Slots != 0 {
		t.Fatalf("no engine calls after a failed dial: %+v", st.Group)
	}
}
