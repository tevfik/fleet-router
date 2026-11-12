package feature

import "testing"

func TestRunner_EnvAndOverride(t *testing.T) {
	for _, c := range []struct {
		env  string
		want bool
	}{{"1", true}, {"on", true}, {"0", false}, {"off", false}} {
		t.Setenv(EnvRunner, c.env)
		if got := detect(); got != c.want {
			t.Errorf("FLEET_RUNNER=%q: detect() = %v, want %v", c.env, got, c.want)
		}
	}
	restore := SetRunner(false)
	if Runner() {
		t.Fatal("SetRunner(false) not honoured")
	}
	inner := SetRunner(true)
	if !Runner() {
		t.Fatal("SetRunner(true) not honoured")
	}
	inner()
	if Runner() {
		t.Fatal("restore did not bring back the previous override")
	}
	restore()
}
