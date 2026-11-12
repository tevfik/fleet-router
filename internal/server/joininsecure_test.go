package server

import (
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestJoinInsecureIsNotCarriedToAnotherHub(t *testing.T) {
	cur := &config.Config{HubURL: "https://old.example/", HubInsecure: true}
	yes, no := true, false
	if !joinInsecure(cur, "https://old.example", nil) {
		t.Error("the configured hub keeps its setting")
	}
	if joinInsecure(cur, "https://other.example", nil) {
		t.Error("another hub must not inherit insecure")
	}
	if !joinInsecure(cur, "https://other.example", &yes) || joinInsecure(cur, "https://old.example", &no) {
		t.Error("an explicit choice wins")
	}
}
