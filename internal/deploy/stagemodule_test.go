package deploy

import "testing"

func TestStageModuleRegistry(t *testing.T) {
	RegisterStageModule(StageModule{Name: "t-mod", Unsupported: []string{"lanes"}})
	m, ok := LookupStageModule("t-mod")
	if !ok || m.Refuses(map[string]bool{"lanes": true}) == nil || m.Refuses(map[string]bool{"np": true}) != nil {
		t.Fatalf("%+v %v", m, ok)
	}
	found := false
	for _, n := range StageModuleNames() {
		found = found || n == "t-mod"
	}
	if !found {
		t.Fatal("not listed")
	}
	if _, ok := LookupStageModule("nothing"); ok {
		t.Fatal("found a module that is not there")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a second registration must panic")
		}
	}()
	RegisterStageModule(StageModule{Name: "t-mod"})
}
