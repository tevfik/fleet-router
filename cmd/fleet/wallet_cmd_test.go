package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// captureOutput captures stdout during function execution.
func captureOutput(f func() error) (string, error) {
	r, w, _ := os.Pipe()
	oldStdout := os.Stdout
	os.Stdout = w

	err := f()

	w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String(), err
}

func TestCmdWalletAddress(t *testing.T) {
	out, err := captureOutput(func() error {
		return cmdWallet([]string{"address"})
	})
	if err != nil {
		t.Fatalf("cmdWallet address failed: %v", err)
	}
	if !strings.Contains(out, "Wallet Address: 0x") {
		t.Errorf("expected 0x wallet address, got:\n%s", out)
	}
	if !strings.Contains(out, "Public Key:     0x") {
		t.Errorf("expected public key in output, got:\n%s", out)
	}
}

func TestCmdWalletStatusNoDeposit(t *testing.T) {
	// credits cannot be made out of nothing: there is no deposit command
	if err := cmdWallet([]string{"deposit", "1250"}); err == nil || !strings.Contains(err.Error(), "unknown wallet command") {
		t.Fatalf("deposit: %v", err)
	}

	// Status / balance
	statusOut, err := captureOutput(func() error {
		return cmdWallet([]string{"status"})
	})
	if err != nil {
		t.Fatalf("cmdWallet status failed: %v", err)
	}
	if !strings.Contains(statusOut, "WALLET & TOKEN ECONOMY") {
		t.Errorf("expected wallet status banner, got:\n%s", statusOut)
	}
}

func TestCmdShareLifecycle(t *testing.T) {
	// 1. Enable sharing
	onOut, err := captureOutput(func() error {
		return cmdShare([]string{"on", "--price", "2000", "--models", "deepseek-v4,qwen2.5"})
	})
	if err != nil {
		t.Fatalf("cmdShare on failed: %v", err)
	}
	if !strings.Contains(onOut, "GPU compute sharing ENABLED") {
		t.Errorf("expected enabled confirmation, got:\n%s", onOut)
	}
	if !strings.Contains(onOut, "2000 credits / 1M tokens") {
		t.Errorf("expected custom price, got:\n%s", onOut)
	}

	// 2. Check status
	stOut, err := captureOutput(func() error {
		return cmdShare([]string{"status"})
	})
	if err != nil {
		t.Fatalf("cmdShare status failed: %v", err)
	}
	if !strings.Contains(stOut, "IDLE GPU COMPUTE SHARING") {
		t.Errorf("expected status header, got:\n%s", stOut)
	}
	if !strings.Contains(stOut, "ACTIVE") {
		t.Errorf("expected active sharing status, got:\n%s", stOut)
	}

	// 3. Disable sharing
	offOut, err := captureOutput(func() error {
		return cmdShare([]string{"off"})
	})
	if err != nil {
		t.Fatalf("cmdShare off failed: %v", err)
	}
	if !strings.Contains(offOut, "GPU compute sharing DISABLED") {
		t.Errorf("expected disabled confirmation, got:\n%s", offOut)
	}
}
