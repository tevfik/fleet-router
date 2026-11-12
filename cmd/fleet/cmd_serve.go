package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/setup"
	"github.com/tevfik/fleet-router/internal/tui"
	"github.com/tevfik/fleet-router/internal/update"
)

func cmdServe() error {
	srv, err := server.New(config.Path())
	if err != nil {
		return err
	}
	if l := os.Getenv("FLEET_LISTEN"); l != "" {
		srv.SetListen(l)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if exe, err := setup.Exe(); err == nil {
		if tag, err := update.OnStart(exe); err != nil {
			fmt.Fprintln(os.Stderr, "rollback failed:", err)
		} else if tag != "" {
			return fmt.Errorf("%s did not stay up — the previous version was put back, restarting", tag)
		}
	}
	go func() { // a version that survived two minutes is good: forget the pending marker
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Minute):
			update.ClearPending()
		}
	}()
	server.Version = version
	// Every machine gets its own hub API key on first start (install time): the key a node has to know to join THIS
	// machine as a hub. It is never printed here; `fleet key show --reveal` or the menu shows it.
	if cfg, cerr := config.Load(config.Path()); cerr == nil {
		if _, generated, kerr := config.EnsureHubKey(cfg); kerr != nil {
			fmt.Fprintln(os.Stderr, "hub API key: could not create one:", kerr)
		} else if generated {
			fmt.Fprintln(os.Stderr, "hub API key: none was set, a new one was generated (fleet key show --reveal, or the menu: Fleet Hub)")
		}
	}
	fmt.Fprintf(os.Stderr, "fleet %s · listening on %s · configuration %s\n", version, srv.Config().Listen, config.Path())
	updated := make(chan string, 1)
	if srv.Config().AutoUpdateOn() && update.Source() != "" { // a build without an update source does not update
		go autoUpdate(ctx, updated, stop)
	}
	err = srv.Run(ctx)
	select {
	case tag := <-updated:
		if os.Getenv("INVOCATION_ID") != "" { // under systemd: a non-zero exit makes Restart=on-failure start the new binary
			return fmt.Errorf("updated to %s — restarting", tag)
		}
		fmt.Fprintf(os.Stderr, "updated to %s — start fleet again to use it\n", tag)
	default:
	}
	return err
}

// autoUpdate checks for a new release a minute after start and then every 6 hours. A new binary is
// installed in place; stop() then ends the daemon so its supervisor restarts it. Errors are only logged.
func autoUpdate(ctx context.Context, updated chan<- string, stop func()) {
	for wait := time.Minute; ; wait = 6 * time.Hour {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		tag, err := updateOnce(cctx, false)
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "auto-update:", err)
			continue
		}
		if tag != "" {
			fmt.Fprintln(os.Stderr, "auto-update: installed", tag)
			updated <- tag
			stop()
			return
		}
	}
}

func cmdTUI() error {
	cl := client.New(listen())
	if cl.Alive() {
		return tui.Run(cl, listen(), false)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	embedded, err := ensureDaemon(daemonProbe{
		alive: cl.Alive,
		start: func() (<-chan error, error) {
			srv, err := server.New(config.Path())
			if err != nil {
				return nil, err
			}
			if l := os.Getenv("FLEET_LISTEN"); l != "" {
				srv.SetListen(l)
			}
			errc := make(chan error, 1)
			go func() { errc <- srv.Run(ctx) }()
			return errc, nil
		},
		sleep: time.Sleep,
		addr:  listen(),
	})
	if err != nil {
		return err
	}
	return tui.Run(cl, listen(), embedded)
}

// daemonProbe is what ensureDaemon needs from the outside: the liveness check, the start of the embedded
// daemon (its Run result arrives on the channel) and the clock, so tests need no real daemon or waiting.
type daemonProbe struct {
	alive func() bool
	start func() (<-chan error, error)
	sleep func(time.Duration)
	addr  string
}

const (
	daemonPoll  = 100 * time.Millisecond
	daemonWait  = 3 * time.Second // how long a daemon that is slow to answer is waited for
	daemonTicks = int(daemonWait / daemonPoll)
)

// ensureDaemon makes sure a daemon answers: one that is already there is used; otherwise the embedded one is
// started. If it cannot bind because the port is taken, a daemon that was only slow to answer the first probe is
// waited for up to daemonWait and used; when nothing answers by then the error says what holds the port. It
// reports whether the embedded daemon is the one in use.
func ensureDaemon(p daemonProbe) (embedded bool, err error) {
	if p.alive() {
		return false, nil
	}
	errc, err := p.start()
	if err != nil {
		return false, err
	}
	for i := 0; i < daemonTicks; i++ {
		select {
		case runErr := <-errc:
			// the port is taken: a daemon that was only slow to answer the first probe is there now; use it
			for j := 0; j < daemonTicks; j++ {
				if p.alive() {
					return false, nil
				}
				p.sleep(daemonPoll)
			}
			if p.alive() {
				return false, nil
			}
			return false, fmt.Errorf("the embedded daemon cannot start (%v) and whatever holds %s did not answer as a fleet daemon within %ds — is it another program, or a daemon that is hung? run `fleet status`, or stop the process on that port", runErr, p.addr, int(daemonWait/time.Second))
		default:
		}
		if p.alive() {
			return true, nil
		}
		p.sleep(daemonPoll)
	}
	if !p.alive() {
		return true, fmt.Errorf("the embedded daemon did not come up on %s within %ds — run `fleet daemon` to see why", p.addr, int(daemonWait/time.Second))
	}
	return true, nil
}
