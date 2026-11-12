//go:build !llamaring

package ringengine

// The default build is pure Go: no engine, no cgo.

func Available() bool                  { return false }
func Version() string                  { return "not built in" }
func RunWorker(args []string) int      { return 127 }
func RunServer(args []string) int      { return 127 }
func RunChainServe(args []string) int  { return 127 }
func RunChainDriver(args []string) int { return 127 }

// Info is empty when the engine is not built in.
func Info() string { return "" }
