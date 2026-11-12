package deploy

import (
	"context"
	"fmt"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// Cross-site chains through the hub (E12b). The same raw TCP stream mode a ring worker is reached through
// (hubstream.go): the stage that dials the next one finds it on 127.0.0.1:Port of its own machine, where a
// `fleet stream connect <next node> <its stage port> --listen 127.0.0.1:Port --hub H` forwards through the hub; the next
// stage's node declares its stage port as a hub stream port. No machine of the deploy sits in the data path (the ssh
// relay of relay.go carries every hop through the deploying machine).

// hubStreamLaunch is the stream connect of relay rel, run on the machine of the stage that dials.
func hubStreamLaunch(rel StageRelay) streamLaunch {
	return streamLaunch{Node: rel.To, Port: rel.ToPort, ListenPort: rel.Port, Hub: rel.Hub}
}

// openHubRelay declares the next stage's port to the hub on its node, then starts the stream connect on the dialing
// stage's machine and waits until it listens. A declaration that did not go through is a warning kept on the runner
// (the port may already be declared through hub_stream_ports); the stream itself failing is an error.
func (r *LiveStageRunner) openHubRelay(rel StageRelay) error {
	ctx, cancel := context.WithTimeout(context.Background(), streamReady+60*time.Second)
	defer cancel()
	var out []byte
	if defaultIsThisMachine(rel.To) {
		out, _ = localScript(ctx, hubJoinScript(rel.Hub, rel.ToPort))
	} else {
		// ~/.local/bin: where fleet is installed (a non-interactive ssh shell lacks it)
		out, _ = sshScript(ctx, rel.To, "export PATH=$HOME/.local/bin:$PATH\n"+hubJoinScript(rel.Hub, rel.ToPort))
	}
	if note := hubJoinNote(rel.To, rel.Hub, rel.ToPort, string(out)); note != "" {
		r.Warnings = append(r.Warnings, note)
	}
	local := defaultIsThisMachine(rel.From)
	apiKey := ""
	if local {
		cfg, _ := config.Load(config.Path())
		apiKey = hubAPIKey(cfg) // only into the environment of a local stream; a remote machine uses its own key
	}
	if _, err := NewRemoteWorkerManager().StartStream(ctx, rel.From, local, hubStreamLaunch(rel), "chain", apiKey); err != nil {
		return fmt.Errorf("hub stream %s -> %s (port %d): %w", rel.From, rel.To, rel.Port, err)
	}
	return nil
}

func hubRelayLine(rel StageRelay) string {
	return fmt.Sprintf("[%s] %s (and on %s: %s)", rel.From, hubStreamLaunch(rel).ShellLine(), rel.To, hubJoinCommand(rel.Hub, rel.ToPort))
}

// configPathOr is path, else the fleet configuration of this user.
func configPathOr(path string) string {
	if path != "" {
		return path
	}
	return config.Path()
}
