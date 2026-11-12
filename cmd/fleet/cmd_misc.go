package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/deploy"
	"github.com/tevfik/fleet-router/internal/vision"
)

// cmdWatchdog watches the ring endpoints and redeploys the ones whose engine reports failed workers.
func cmdWatchdog(args []string) error {
	fs := flag.NewFlagSet("watchdog", flag.ExitOnError)
	interval := fs.Duration("interval", 30*time.Second, "time between checks")
	once := fs.Bool("once", false, "check once and exit")
	maxRestarts := fs.Int("max-restarts", 3, "restarts of one endpoint before giving up (0: no limit)")
	_ = fs.Parse(args)
	cfg, err := config.Load(config.Path())
	if err != nil {
		return err
	}
	w := deploy.NewWatchdog(cfg)
	w.MaxRestarts = *maxRestarts
	w.SkipChains = cfg.ChainSupervision() // the daemon supervises the chains (supervise_chains)
	// read again on every pass: an endpoint deployed after the watchdog started is watched too
	w.Config = func() *config.Config {
		if c, err := config.Load(config.Path()); err == nil {
			return c
		}
		return cfg
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *once {
		w.CheckOnce(ctx)
		return nil
	}
	w.Run(ctx, *interval)
	return nil
}

// cmdVision handles modular vision microservice commands (serve, describe, status).
func cmdVision(args []string) error {
	if len(args) == 0 || args[0] == "help" {
		fmt.Println("usage: fleet vision [serve|describe|status] ...")
		fmt.Println("  fleet vision serve [--model <mmproj.gguf>] [--port 18095] [--cpu] [--backend <url>]")
		fmt.Println("  fleet vision describe <image.jpg> [--prompt <text>] [--url <service_url>]")
		fmt.Println("  fleet vision status [--url <service_url>]")
		return nil
	}
	switch args[0] {
	case "serve":
		fs := flag.NewFlagSet("vision serve", flag.ExitOnError)
		model := fs.String("model", "", "path to multimodal projector or vision GGUF")
		port := fs.Int("port", vision.DefaultVisionPort, "port to listen on")
		cpu := fs.Bool("cpu", true, "run multimodal projector on CPU (zero GPU VRAM)")
		backend := fs.String("backend", "", "optional backend multimodal engine URL")
		fs.Parse(args[1:])

		srv := vision.NewServer(vision.ServerOptions{
			Port:       *port,
			ModelPath:  *model,
			CPUOnly:    *cpu,
			BackendURL: *backend,
		})
		fmt.Printf("✓ fleet vision microservice starting on :%d (cpu=%v, model=%s)\n", *port, *cpu, *model)
		return srv.Start()

	case "describe":
		fs := flag.NewFlagSet("vision describe", flag.ExitOnError)
		prompt := fs.String("prompt", "", "custom prompt for description")
		url := fs.String("url", "", "vision microservice URL")
		fs.Parse(args[1:])
		if fs.NArg() < 1 {
			return fmt.Errorf("usage: fleet vision describe <image-file|url> [--prompt P] [--url U]")
		}
		imgPath := fs.Arg(0)
		cl := vision.NewClient(*url)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		var imgData string
		if strings.HasPrefix(imgPath, "http://") || strings.HasPrefix(imgPath, "https://") || strings.HasPrefix(imgPath, "data:image/") {
			imgData = imgPath
		} else {
			data, err := os.ReadFile(imgPath)
			if err != nil {
				return err
			}
			mime := "image/jpeg"
			if strings.HasSuffix(strings.ToLower(imgPath), ".png") {
				mime = "image/png"
			}
			imgData = fmt.Sprintf("data:%s;base64,%s", mime, base64.StdEncoding.EncodeToString(data))
		}

		desc, err := cl.Describe(ctx, imgData, *prompt)
		if err != nil {
			return err
		}
		fmt.Println(desc)
		return nil

	case "status":
		fs := flag.NewFlagSet("vision status", flag.ExitOnError)
		url := fs.String("url", "", "vision microservice URL")
		fs.Parse(args[1:])
		cl := vision.NewClient(*url)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if cl.Ping(ctx) {
			fmt.Printf("✓ fleet vision microservice is online at %s\n", cl.BaseURL)
		} else {
			fmt.Printf("✗ fleet vision microservice is unreachable at %s\n", cl.BaseURL)
		}
		return nil

	default:
		return fmt.Errorf("unknown vision command: %s", args[0])
	}
}

// cmdRunOn runs a model directly on a single device (local or remote).
func cmdRunOn(args []string) error {
	fs := flag.NewFlagSet("run-on", flag.ExitOnError)
	port := fs.Int("port", 18092, "port for the server")
	ctxLen := fs.Int("ctx", 4096, "context length")
	ngl := fs.Int("ngl", 99, "GPU layers to offload")
	dryRun := fs.Bool("dry-run", false, "dry run without starting")
	register := fs.Bool("register", false, "also register the server as an endpoint (fleet endpoint list; stop it again with fleet endpoint stop <name>)")
	fs.Parse(flagsFirst(args, "port", "ctx", "ngl", "dry-run"))

	if fs.NArg() < 2 {
		return fmt.Errorf("usage: fleet run-on <node> <model.gguf> [--port N] [--ctx N] [--ngl N] [--register] [--dry-run]")
	}
	node, modelPath := fs.Arg(0), fs.Arg(1)

	cfg, _ := config.Load(config.Path())
	res, err := deploy.RunOnNode(context.Background(), cfg, node, modelPath, *port, *ctxLen, *ngl, *dryRun)
	if err != nil {
		return err
	}
	switch {
	case *dryRun:
		fmt.Printf("dry run, on [%s] it would start:\n  %s\n  endpoint %s at %s\n", node, res.MasterCmd, res.EndpointName, res.URL)
		if res.MasterEngine != "" {
			fmt.Printf("  engine: %s\n", res.MasterEngine)
		}
	case res.Status == deploy.StatusRunning:
		fmt.Printf("✓ Model running on [%s]: %s (PID %d)\n", node, res.URL, res.MasterPID)
	default:
		fmt.Printf("… Model starting on [%s]: %s (PID %d)\n", node, res.URL, res.MasterPID)
	}
	for _, w := range res.Warnings {
		fmt.Printf("WARNING: %s\n", w)
	}
	if *dryRun {
		return nil
	}
	if *register {
		name, err := registerRunOn(node, modelPath, res, *ctxLen)
		if err != nil {
			return fmt.Errorf("the server runs, but registering it failed: %w", err)
		}
		fmt.Printf("✓ registered as endpoint %s (fleet probe %s: what it can do; fleet role set <role> %s: route to it; fleet endpoint stop %s --remove: stop it and forget it)\n", name, name, name, name)
	} else {
		fmt.Printf("not registered: add --register, or fleet endpoint add %s %s --node %s\n", res.EndpointName, res.URL, node)
	}
	return nil
}
