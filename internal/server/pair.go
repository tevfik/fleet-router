package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// Pairing lets a fleet found on the network become a peer without copying API keys around: on the machine
// to be added, `fleet peers pair` shows a 6-digit code (valid 10 minutes, one use); on the other one,
// `fleet peers add <name> http://<ip>:4711/v1 pair=<code>` sends the code, and the fleet that showed it
// answers with a key made for this peer alone. It keeps only the key's hash (paired:), and accepts the key
// wherever its API key is accepted. Five wrong codes end the code. The code is what proves the two
// sides are the machines the user is looking at.

const (
	pairValid    = 10 * time.Minute
	pairMaxTries = 5
)

type pairState struct {
	mu    sync.Mutex
	code  string
	until time.Time
	tries int
}

// PairCode is shown by `fleet peers pair`.
type PairCode struct {
	Code  string    `json:"code"`
	Until time.Time `json:"until"`
	URLs  []string  `json:"urls"` // how the other machine can reach this fleet
	Note  string    `json:"note,omitempty"`
}

// NewPairCode makes a fresh code (the previous one stops working).
func (s *Server) NewPairCode() PairCode {
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	p := &s.pair
	p.mu.Lock()
	p.code, p.until, p.tries = fmt.Sprintf("%06d", n.Int64()), time.Now().Add(pairValid), 0
	pc := PairCode{Code: p.code, Until: p.until}
	p.mu.Unlock()
	c := s.Config()
	host, port, _ := net.SplitHostPort(c.Listen)
	if ip := net.ParseIP(host); host != "" && ip != nil && ip.IsLoopback() {
		pc.Note = "this fleet listens on " + c.Listen + " only — set listen: 0.0.0.0:" + port + " so the other machine can reach it"
	}
	for _, a := range lanAddrs() {
		pc.URLs = append(pc.URLs, "http://"+net.JoinHostPort(a, port)+"/v1")
	}
	return pc
}

func lanAddrs() []string {
	var out []string
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 || strings.HasPrefix(i.Name, "docker") || strings.HasPrefix(i.Name, "br-") || strings.HasPrefix(i.Name, "veth") {
			continue
		}
		as, _ := i.Addrs()
		for _, a := range as {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
				out = append(out, n.IP.String())
			}
		}
	}
	return out
}

type pairRequest struct {
	Code string `json:"code"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

type pairAnswer struct {
	Key  string `json:"key"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// acceptPair checks a code and, when it is right, registers the asking fleet and returns its key.
func (s *Server) acceptPair(in pairRequest) (pairAnswer, int, error) {
	p := &s.pair
	p.mu.Lock()
	switch {
	case p.code == "" || time.Now().After(p.until):
		p.mu.Unlock()
		return pairAnswer{}, 403, fmt.Errorf("no pairing code is open here — run `fleet peers pair` on this machine")
	case subtle.ConstantTimeCompare([]byte(strings.TrimSpace(in.Code)), []byte(p.code)) != 1:
		p.tries++
		if p.tries >= pairMaxTries {
			p.code = ""
		}
		p.mu.Unlock()
		return pairAnswer{}, 403, fmt.Errorf("wrong pairing code")
	}
	p.code = "" // one use
	p.mu.Unlock()
	if in.ID == "" {
		return pairAnswer{}, 400, fmt.Errorf("the asking fleet sent no id")
	}
	raw := make([]byte, 32)
	rand.Read(raw)
	key := "fleet-" + hex.EncodeToString(raw)
	c := s.Config()
	var ps []config.Paired
	for _, x := range c.Paired {
		if x.ID != in.ID { // pairing again replaces the old key
			ps = append(ps, x)
		}
	}
	c.Paired = append(ps, config.Paired{ID: in.ID, Name: in.Name, KeyHash: keyHash(key), At: time.Now()})
	if err := s.SetConfig(c); err != nil {
		return pairAnswer{}, 500, err
	}
	return pairAnswer{Key: key, ID: c.ID, Name: c.Name}, 200, nil
}

func keyHash(k string) string {
	h := sha256.Sum256([]byte(k))
	return hex.EncodeToString(h[:])
}

// pairedKey reports whether a bearer token is the key of a paired fleet.
func (s *Server) pairedKey(tok string) bool {
	if tok == "" {
		return false
	}
	h := keyHash(tok)
	for _, p := range s.Config().Paired {
		if subtle.ConstantTimeCompare([]byte(h), []byte(p.KeyHash)) == 1 {
			return true
		}
	}
	return false
}

func (s *Server) pairHandler(w http.ResponseWriter, r *http.Request) {
	var in pairRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	a, code, err := s.acceptPair(in)
	if err != nil {
		httpErr(w, code, err.Error())
		return
	}
	writeJSON(w, a)
}

// pairWith sends a code to the fleet at url (…/v1) and returns the key it gave.
func (s *Server) pairWith(ctx context.Context, url, code string) (string, error) {
	c := s.Config()
	body, _ := json.Marshal(pairRequest{Code: code, ID: c.ID, Name: c.Name})
	cx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cx, "POST", strings.TrimSuffix(strings.TrimRight(url, "/"), "/v1")+"/fleet/pair", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("pairing refused: %s", errMessage(b))
	}
	var a pairAnswer
	if err := json.Unmarshal(b, &a); err != nil || a.Key == "" {
		return "", fmt.Errorf("pairing: no key in the answer")
	}
	return a.Key, nil
}
