package economy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A ledger file that cannot be parsed is kept (moved aside), not overwritten by the next Save.
func TestCorruptLedgerIsKeptNotOverwritten(t *testing.T) {
	p := filepath.Join(t.TempDir(), "economy.json")
	if err := os.WriteFile(p, []byte(`{"accounts": {"x": {"balance": 5`), 0600); err != nil {
		t.Fatal(err)
	}
	l, err := LoadLedger(p)
	if err != nil || l == nil {
		t.Fatalf("%v %v", l, err)
	}
	if _, err := l.Deposit("addr", 10); err != nil {
		t.Fatal(err)
	}
	if err := l.Save(p); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(p + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("the corrupt file must be kept aside: %v", matches)
	}
	if b, _ := os.ReadFile(matches[0]); !strings.Contains(string(b), `"balance": 5`) {
		t.Fatalf("the old content must be intact: %s", b)
	}
}

// An unreadable ledger returns a usable (non-nil) ledger and an error, and Save refuses to overwrite the file.
func TestUnreadableLedgerNeverNilAndNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "economy.json")
	if err := os.Mkdir(p, 0700); err != nil { // a directory where the file should be: ReadFile fails (not "not exist")
		t.Fatal(err)
	}
	l, err := LoadLedger(p)
	if err == nil || l == nil {
		t.Fatalf("want a ledger and an error, got %v %v", l, err)
	}
	_ = l.GetOrCreateAccount("a") // must not panic
	if err := l.Save(p); err == nil {
		t.Fatal("Save must refuse after a failed load")
	}
}

func TestSaveLeavesNoTempFilesAndIs0600(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "economy.json")
	l := NewLedger()
	_, _ = l.Deposit("a", 1)
	if err := l.Save(p); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("%v %v", st, err)
	}
	if m, _ := filepath.Glob(p + ".tmp-*"); len(m) != 0 {
		t.Fatalf("temp files left: %v", m)
	}
}
