package tunnel

import "testing"

func TestOkForwardTarget(t *testing.T) {
	for _, ok := range [][2]string{{"POST", "/v1/chat/completions"}, {"GET", "/v1/models"}} {
		if err := okForwardTarget(ok[0], ok[1]); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	for _, bad := range [][2]string{
		{"POST", "@evil.host/x"}, {"POST", "/fleet/config"}, {"POST", "/v1/../fleet/apikeys"}, {"POST", "/v1/x?y=1"},
		{"POST", "/v1/a@b"}, {"DELETE", "/v1/models"}, {"POST", ""}, {"POST", "/v1"},
	} {
		if okForwardTarget(bad[0], bad[1]) == nil {
			t.Errorf("%v must be refused", bad)
		}
	}
}
