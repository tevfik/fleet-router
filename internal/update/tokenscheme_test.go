package update

import (
	"net/url"
	"testing"
)

func TestTokenOnlyOverHTTPSOrLoopback(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://git.example/x":   true,
		"http://git.example/x":    false,
		"http://10.0.0.5:3000/x":  false,
		"http://127.0.0.1:8080/x": true,
		"http://localhost:8080/x": true,
		"http://[::1]:8080/x":     true,
		"ftp://git.example/x":     false,
	} {
		u, _ := url.Parse(raw)
		if got := tokenMaySendTo(u); got != want {
			t.Errorf("%s: %v, want %v", raw, got, want)
		}
	}
}
