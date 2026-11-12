package server

import (
	"context"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/deploy"
)

// chainSuperviseEvery: how often the chains this daemon deployed are looked at (two bad looks in a row restart a
// dead stage, so a stage is back within about two minutes plus its load time).
var chainSuperviseEvery = time.Minute

// chainSuperviseLoop restarts the dead stages of the chains deployed from this machine (deploy.Watchdog, chains
// only; supervise_chains: false turns it off). The stages run outside the daemon (their own scope), so a daemon
// restart does not touch them; this loop only acts on a stage that is gone.
func (s *Server) chainSuperviseLoop(ctx context.Context) {
	w := deploy.NewWatchdog(s.Config())
	w.ChainsOnly = true
	w.Config = func() *config.Config {
		c := s.Config()
		if !c.ChainSupervision() {
			return &config.Config{}
		}
		return c
	}
	t := time.NewTicker(chainSuperviseEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		w.CheckOnce(ctx)
	}
}
