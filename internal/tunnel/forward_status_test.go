package tunnel

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A node whose forward fails (here: a path it refuses) ends the request with an error, not an empty 200.
func TestForwardRequestNodeErrorIsAnError(t *testing.T) {
	hub, _, _, _ := tunnelHubWithBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("x")) }))
	req := httptest.NewRequest("POST", "/fleet/admin", strings.NewReader("x")) // not /v1/...: the node refuses it
	rec := httptest.NewRecorder()
	err := hub.ForwardRequest("n1", req, rec)
	if err == nil || !strings.Contains(err.Error(), "not forwarded") {
		t.Fatalf("err %v, code %d body %q", err, rec.Code, rec.Body.String())
	}
	if errors.Is(err, ErrResponseStarted) {
		t.Fatal("nothing was written: the caller may still fail over")
	}
}

// A 5xx or 429 from the node's model server is not written to the client: the caller gets an error (with the
// status and the body) and can try another endpoint.
func TestForwardRequestUpstream5xxFailsOver(t *testing.T) {
	for _, code := range []int{http.StatusBadGateway, http.StatusTooManyRequests} {
		hub, _, _, _ := tunnelHubWithBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			w.Write([]byte(`{"error":"model is loading"}`))
		}))
		rec := httptest.NewRecorder()
		err := hub.ForwardRequest("n1", httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}")), rec)
		var us *UpstreamStatusError
		if !errors.As(err, &us) || us.Status != code || !strings.Contains(us.Body, "model is loading") {
			t.Fatalf("%d: err %v", code, err)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("%d: written to the client: %q", code, rec.Body.String())
		}
	}
	// a 4xx other than 429 is the client's own mistake: passed through
	hub, _, _, _ := tunnelHubWithBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad"))
	}))
	rec := httptest.NewRecorder()
	if err := hub.ForwardRequest("n1", httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}")), rec); err != nil || rec.Code != 400 {
		t.Fatalf("400: %v %d", err, rec.Code)
	}
}

// The limit is on silence, not on the whole answer: a stream that keeps sending outlives it.
func TestForwardRequestIdleLimitNotTotal(t *testing.T) {
	old := tunnelIdleLimit
	tunnelIdleLimit = 400 * time.Millisecond
	t.Cleanup(func() { tunnelIdleLimit = old })
	hub, _, _, _ := tunnelHubWithBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for i := 0; i < 12; i++ { // 1.2 s in all, never more than 100 ms quiet
			w.Write([]byte("tok "))
			fl.Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	rec := httptest.NewRecorder()
	if err := hub.ForwardRequest("n1", httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}")), rec); err != nil {
		t.Fatalf("a flowing stream was cut: %v", err)
	}
	if strings.Count(rec.Body.String(), "tok") != 12 {
		t.Fatalf("%q", rec.Body.String())
	}
	hold := make(chan struct{})
	hub2, _, _, _ := tunnelHubWithBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-hold }))
	t.Cleanup(func() { close(hold) }) // after hub2's: cleanups run last first, and its server waits for this handler
	t0 := time.Now()
	if err := hub2.ForwardRequest("n1", httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}")), httptest.NewRecorder()); err == nil || time.Since(t0) > 3*time.Second {
		t.Fatalf("a silent node: %v after %s", err, time.Since(t0))
	}
}
