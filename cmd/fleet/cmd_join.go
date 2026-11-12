package main

import (
	"flag"
	"fmt"

	"github.com/tevfik/fleet-router/internal/config"
)

// cmdLeave takes this machine out of the line (or, with --back, puts it in again). It only ever acts on the
// machine the daemon runs on: nobody can send another machine away.
func cmdLeave(args []string) error {
	cl, err := mustClient()
	if err != nil {
		return err
	}
	back := len(args) > 0 && args[0] == "--back"
	if err := cl.Drain(!back); err != nil {
		return err
	}
	if back {
		fmt.Println("✓ back in the line")
	} else {
		fmt.Println("✓ left the line: plans skip this machine, its models come last, groups it belongs to are told (fleet leave --back to rejoin)")
	}
	return nil
}

// cmdJoin connects this node to a Fleet Hub (e.g. https://hub.example.com) over an outbound WebSocket tunnel.
func cmdJoin(args []string) error {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	token := fs.String("token", "", "optional auth token for joining hub")
	leave := fs.Bool("leave", false, "disconnect from the active hub")
	var streamPorts portList
	fs.Var(&streamPorts, "stream-port", "local TCP port the hub may open raw streams to (repeatable; default: hub_stream_ports of the config); replaces the remembered list")
	var addPorts portList
	fs.Var(&addPorts, "add-stream-port", "like --stream-port, but added to the ports already allowed (hub_stream_ports) instead of replacing them; without --token the remembered token of the same hub is kept")
	insecure := fs.Bool("insecure", false, "do not verify the hub's TLS certificate (a self-signed hub); remembered as hub_insecure")
	secure := fs.Bool("secure", false, "verify the hub's TLS certificate again (clears hub_insecure)")
	fs.Parse(flagsFirst(args, "token", "stream-port", "add-stream-port"))
	if *insecure && *secure {
		return fmt.Errorf("--insecure and --secure exclude each other")
	}
	cl, err := mustClient()
	if err != nil {
		return err
	}
	if *leave {
		if err := cl.LeaveHub(); err != nil {
			return err
		}
		fmt.Println("✓ Disconnected from Fleet Hub")
		return nil
	}
	hubURL, err := config.HubOrDefault(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("%w\nusage: fleet join <hub URL> [--token T]", err)
	}
	if len(addPorts) > 0 && len(streamPorts) > 0 {
		return fmt.Errorf("use either --stream-port (replace the list) or --add-stream-port (add to it), not both")
	}
	if *insecure || *secure {
		err = cl.JoinHubInsecure(hubURL, *token, *insecure, streamPorts, addPorts)
	} else if len(addPorts) > 0 {
		err = cl.JoinHubAdd(hubURL, *token, addPorts)
	} else {
		err = cl.JoinHub(hubURL, *token, streamPorts...)
	}
	if err != nil {
		return fmt.Errorf("failed to join hub %s: %w", hubURL, err)
	}
	fmt.Printf("✓ Connected to Fleet Hub at %s over WebSocket tunnel\n", hubURL)
	if *insecure {
		fmt.Println("  ! the hub's TLS certificate is NOT verified (hub_insecure); drop it with fleet join --secure once the hub has a valid certificate")
	}
	if len(streamPorts) > 0 {
		fmt.Printf("  streams allowed to local port(s) %s (fleet stream connect <this node> <port> on the master)\n", streamPorts.String())
	}
	if len(addPorts) > 0 {
		fmt.Printf("  streams also allowed to local port(s) %s (added to hub_stream_ports)\n", addPorts.String())
	}
	return nil
}
