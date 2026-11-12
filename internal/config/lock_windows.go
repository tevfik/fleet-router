//go:build windows

package config

// Lock is a no-op on Windows (fleet runs on Linux; this keeps the package building).
func Lock(string) (func(), error) { return func() {}, nil }
