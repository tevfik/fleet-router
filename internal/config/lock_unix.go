//go:build !windows

package config

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// Lock takes an exclusive advisory lock next to the configuration file (path + ".lock") so that two processes that
// load, change and save it (`fleet deploy`, the daemon, a second deploy) do not overwrite each other's change.
// It waits up to 15 s; the returned func releases it. Advisory: it protects only writers that call Lock.
func Lock(path string) (func(), error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("the configuration %s is locked by another fleet process: %w", path, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
