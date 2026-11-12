package economy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type persistentState struct {
	Accounts     map[string]*Account          `json:"accounts"`
	SettledTx    map[string]*InferenceReceipt `json:"settled_tx"`
	ShareConfigs map[string]*ShareConfig      `json:"share_configs"`
}

// DefaultStatePath returns ~/.config/fleet/economy.json
func DefaultStatePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "fleet", "economy.json")
}

// LoadLedger loads persistent accounts, settled receipts, and share configurations.
func LoadLedger(path string) (*Ledger, error) {
	if path == "" {
		path = DefaultStatePath()
	}

	l := NewLedger()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		// Never a nil ledger (callers that drop the error would panic), and one that is not saved over a file that
		// exists but could not be read.
		l.noSave = fmt.Errorf("ledger %s was not loaded: %w", path, err)
		return l, err
	}

	var state persistentState
	if err := json.Unmarshal(data, &state); err != nil {
		// Unparseable: keep the file (a later Save would overwrite every balance and receipt) and start empty.
		bak := fmt.Sprintf("%s.corrupt-%s", path, time.Now().Format("20060102-150405"))
		if rerr := os.Rename(path, bak); rerr != nil {
			l.noSave = fmt.Errorf("ledger %s is corrupt (%v) and could not be moved aside: %w", path, err, rerr)
			return l, l.noSave
		}
		return l, nil
	}

	if state.Accounts != nil {
		l.accounts = state.Accounts
		// Holds live in memory only: after a restart no hold exists to settle or release them, so the credits that
		// were locked go back to the balance (they used to stay locked for good).
		for _, a := range l.accounts {
			if a != nil && a.Locked > 0 {
				a.Balance += a.Locked
				a.Locked = 0
			}
		}
	}
	if state.SettledTx != nil {
		l.settledTx = state.SettledTx
	}
	if state.ShareConfigs != nil {
		l.shareConfigs = state.ShareConfigs
	}

	return l, nil
}

// Save persists ledger state to disk atomically.
func (l *Ledger) Save(path string) error {
	if l.noSave != nil {
		return l.noSave
	}
	if path == "" {
		path = DefaultStatePath()
	}

	l.mu.RLock()
	state := persistentState{
		Accounts:     l.accounts,
		SettledTx:    l.settledTx,
		ShareConfigs: l.shareConfigs,
	}
	data, err := json.MarshalIndent(state, "", "  ")
	l.mu.RUnlock()

	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}

	// A unique temp name (two savers must not interleave in one file) and an fsync before the rename (a crash must
	// not leave an empty ledger behind).
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return werr
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
