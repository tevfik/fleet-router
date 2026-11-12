//go:build llamaring

// Package ringengine links llama-ring (llama.cpp plus the ring backend, the submodule third_party/fleet-engine) into
// fleet through cgo, statically: one executable, no llama/ggml shared libraries beside it. `make build-engine` builds it.
//
// Both entry points are process-wide (signal handlers, exit() on fatal errors, exit() on fatal errors), so fleet
// runs them in a child process: the same binary re-executed as `fleet ring worker|serve …`, never in the daemon.
package ringengine

/*
#cgo CFLAGS: -I${SRCDIR}/../../third_party/fleet-engine/include
#include <stdlib.h>
#include "ring_engine.h"
*/
import "C"

import "unsafe"

func Available() bool { return true }

// Info is the engine's identity as JSON: os, arch, backend (cpu/cuda/metal/vulkan), cuda_archs, protocol.
func Info() string { return C.GoString(C.ring_engine_info()) }

// Version describes the linked engine.
func Version() string { return C.GoString(C.ring_engine_version()) }

func argv(prog string, args []string) (C.int, **C.char, func()) {
	all := append([]string{prog}, args...)
	arr := (**C.char)(C.malloc(C.size_t(len(all)+1) * C.size_t(unsafe.Sizeof(uintptr(0)))))
	sl := unsafe.Slice(arr, len(all)+1)
	for i, a := range all {
		sl[i] = C.CString(a)
	}
	sl[len(all)] = nil
	return C.int(len(all)), arr, func() {
		for i := range all {
			C.free(unsafe.Pointer(sl[i]))
		}
		C.free(unsafe.Pointer(arr))
	}
}

// RunWorker serves this machine's backends to a ring master (llama-ring's ring-worker); it blocks. args are the
// tool's options, e.g. -H 127.0.0.1 -p 50152 -d CUDA0 -c (the weight cache directory comes from LLAMA_CACHE).
func RunWorker(args []string) int {
	n, a, free := argv("ring-worker", args)
	defer free()
	return int(C.ring_worker(n, a))
}

// RunServer runs llama.cpp's llama-server with the ring backend linked in; it blocks until stopped. RING_SERVERS=
// host:port,… in the environment adds the workers as devices RING0.. for -dev and -ts.
func RunServer(args []string) int {
	n, a, free := argv("llama-server", args)
	defer free()
	return int(C.ring_server(n, a))
}

// RunChainServe runs a single pipeline stage (contiguous layer range [B,E)) behind Fleet Stage Protocol v1 (FSP1).
func RunChainServe(args []string) int {
	n, a, free := argv("fleet-engine-stage-serve", args)
	defer free()
	return int(C.chain_serve(n, a))
}

// RunChainDriver runs the FSP1 stage-chain driver across N stage servers.
func RunChainDriver(args []string) int {
	n, a, free := argv("fleet-engine-stage-chain", args)
	defer free()
	return int(C.chain_run(n, a))
}
