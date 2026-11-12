package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tevfik/fleet-router/internal/ggufhead"
	"github.com/tevfik/fleet-router/internal/ringengine"
)

// cmdRing is the entry the embedded llama-ring engine runs behind: `fleet ring worker|serve …`. A group's
// processes are this same binary started with these subcommands (by hand, by systemd, or by fleet itself). `serve` with RING_STATS_PORT=<port> in its environment answers
// GET 127.0.0.1:<port>/ring/trace (per-token profile as JSON) and /ring/info, so a
// machine needs nothing but fleet to take part in a model. Everything after the subcommand goes to the engine
// unchanged (`fleet ring serve -m model.gguf -dev RING1,CUDA0 -ts 1,3 --port 18090`).
func cmdRing(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: fleet ring version | info | worker [ring-worker options] | serve [llama-server options]")
	}
	if args[0] == "version" {
		if ringengine.Available() {
			fmt.Println(ringengine.Version())
			return nil
		}
		if eng, ok := ringengine.FindEnginePath(); ok {
			cmd := exec.Command(eng, "ring", "version")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		}
		fmt.Println(ringengine.Version())
		return nil
	}
	if !ringengine.Available() {
		if eng, ok := ringengine.FindEnginePath(); ok {
			cmd := exec.Command(eng, append([]string{"ring"}, args...)...)
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Env = os.Environ()
			if err := cmd.Run(); err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					os.Exit(exitErr.ExitCode())
				}
				return err
			}
			os.Exit(0)
		}
		return ringengine.ErrNotBuilt
	}
	if args[0] == "info" { // machine-readable identity, for choosing the right engine variant per node
		fmt.Println(ringengine.Info())
		return nil
	}
	switch args[0] {
	case "worker":
		os.Exit(ringengine.RunWorker(args[1:]))
	case "serve":
		os.Exit(ringengine.RunServer(args[1:]))
	}
	return fmt.Errorf("unknown ring command %q (version, info, worker, serve)", args[0])
}

// cmdChain is Protocol 2 (FSP1 stage chain): `fleet chain serve|run …` (also reachable as `fleet stage …`).
// When built with `make build-engine` (`-tags llamaring`), it runs the embedded C++ FSP1 stage server or driver;
// otherwise it delegates to a standalone `fleet-engine-stage` (or `fleet-engine`) binary on disk.
// stageInvocation turns a start as fleet-engine-stage (argv[0]) into the `fleet chain` command line: serve / chain / run
// are the stage modes of the embedded engine, the standalone program took them as its first argument too.
func stageInvocation(argv0 string, args []string) []string {
	if filepath.Base(argv0) != "fleet-engine-stage" {
		return args
	}
	return append([]string{"chain"}, args...)
}

func cmdChain(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: fleet chain version | info | serve [stage-serve options] | run [stage-chain options] | dflash-head DRAFT TARGET OUT")
	}
	if args[0] == "version" {
		if ringengine.Available() {
			fmt.Println(ringengine.Version())
			return nil
		}
		if eng, ok := ringengine.FindChainPath(); ok && filepath.Base(eng) == "fleet-engine" {
			cmd := exec.Command(eng, "chain", "version")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		}
		fmt.Println(ringengine.Version())
		return nil
	}
	if args[0] == "dflash-head" {
		return cmdDFlashHead(args[1:])
	}
	if args[0] == "info" {
		if ringengine.Available() {
			fmt.Println(ringengine.Info())
			return nil
		}
		if eng, ok := ringengine.FindChainPath(); ok && filepath.Base(eng) == "fleet-engine" {
			cmd := exec.Command(eng, "chain", "info")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		}
		fmt.Println(ringengine.Info())
		return nil
	}
	if !ringengine.Available() {
		if eng, ok := ringengine.FindChainPath(); ok {
			var cmdArgs []string
			if filepath.Base(eng) == "fleet-engine-stage" {
				// fleet-engine-stage expects "serve" or "chain" (or "run") directly
				cmdArgs = args
				if len(cmdArgs) > 0 && cmdArgs[0] == "run" {
					cmdArgs = append([]string{"chain"}, cmdArgs[1:]...)
				}
				// Ensure LD_LIBRARY_PATH includes the directory of fleet-engine-stage for libllama.so / libggml.so
				env := os.Environ()
				binDir := filepath.Dir(eng)
				if cur := os.Getenv("LD_LIBRARY_PATH"); cur != "" {
					env = append(env, "LD_LIBRARY_PATH="+binDir+":"+cur)
				} else {
					env = append(env, "LD_LIBRARY_PATH="+binDir)
				}
				cmd := exec.Command(eng, cmdArgs...)
				cmd.Stdin = os.Stdin
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				cmd.Env = env
				if err := cmd.Run(); err != nil {
					if exitErr, ok := err.(*exec.ExitError); ok {
						os.Exit(exitErr.ExitCode())
					}
					return err
				}
				os.Exit(0)
			}
			cmd := exec.Command(eng, append([]string{"chain"}, args...)...)
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Env = os.Environ()
			if err := cmd.Run(); err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					os.Exit(exitErr.ExitCode())
				}
				return err
			}
			os.Exit(0)
		}
		return ringengine.ErrNotBuilt
	}
	switch args[0] {
	case "serve":
		os.Exit(ringengine.RunChainServe(args[1:]))
	case "run", "chain":
		os.Exit(ringengine.RunChainDriver(args[1:]))
	}
	return fmt.Errorf("unknown chain command %q (version, info, serve, run)", args[0])
}

// cmdDFlashHead copies the target's LM head into a DFlash draft so the draft can run next to chain stage 0,
// which has no head (`fleet-engine-stage chain --dflash-draft OUT` with more than one stage).
func cmdDFlashHead(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: fleet chain dflash-head DRAFT.gguf TARGET.gguf (any shard) OUT.gguf")
	}
	res, err := ggufhead.EmbedHead(args[0], args[1], args[2])
	if err != nil {
		return err
	}
	fmt.Printf("%s: added %s (%.0f MiB from the target, bytes unchanged)\n", args[2], strings.Join(res.Tensors, ", "), float64(res.Bytes)/(1<<20))
	return nil
}
