package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/tevfik/agenttui"
	"github.com/tevfik/fleet-router/internal/client"
)

func cmdAsk(args []string) error {
	args = flagsFirst(args, "s", "f", "n", "i")
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	var system, file string
	var maxTokens int
	var interactive bool
	fs.StringVar(&system, "s", "", "system prompt")
	fs.StringVar(&file, "f", "", "file to append")
	fs.IntVar(&maxTokens, "n", 0, "max tokens")
	fs.BoolVar(&interactive, "i", false, "interactive TUI chat via agenttui")

	if err := fs.Parse(args); err != nil {
		return err
	}

	rem := fs.Args()
	role := ""
	if len(rem) >= 1 {
		role = rem[0]
	}
	var textArgs []string
	if len(rem) > 1 {
		textArgs = rem[1:]
	}

	var p string
	if len(textArgs) > 0 {
		p = strings.Join(textArgs, " ")
	}

	stat, err := os.Stdin.Stat()
	isNotTTY := err == nil && stat.Mode()&os.ModeCharDevice == 0

	if p == "-" || (len(textArgs) == 0 && isNotTTY && len(rem) >= 1) {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		p = string(b)
	}

	if file != "" {
		f, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if p != "" {
			p += "\n\n" + string(f)
		} else {
			p = string(f)
		}
	}

	c, err := mustClient()
	if err != nil {
		return err
	}

	// Interactive agenttui mode: when explicitly requested (-i) or when run with no prompt in a TTY with a role
	if interactive || (len(rem) >= 1 && p == "" && !isNotTTY) {
		return runAskAgentTUI(c, role, system, maxTokens, p)
	}

	if len(rem) < 1 {
		return fmt.Errorf("usage: fleet ask [-s system] [-f file] [-n maxTokens] [-i] <role> [text… | -]")
	}

	if p == "" {
		return fmt.Errorf("empty prompt")
	}

	answer, endpoint, err := c.Chat(role, system, p, maxTokens, 10*time.Minute)
	if err != nil {
		return err
	}

	extra := endpoint
	st, _ := c.Status()
	for _, ep := range st.Endpoints {
		if (ep.Name == endpoint || ep.Ref() == endpoint) && ep.TPS > 0 {
			extra = fmt.Sprintf("%s · %.1f t/s", endpoint, ep.TPS)
			break
		}
	}
	fmt.Fprintf(os.Stderr, "[%s]\n", extra)
	fmt.Println(answer)
	return nil
}

var fleetBanner = []string{
	"  ███████╗██╗     ███████╗███████╗████████╗",
	"  ██╔════╝██║     ██╔════╝██╔════╝╚══██╔══╝",
	"  █████╗  ██║     █████╗  █████╗     ██║   ",
	"  ██╔══╝  ██║     ██╔══╝  ██╔══╝     ██║   ",
	"  ██║     ███████╗███████╗███████╗   ██║   ",
	"  ╚═╝     ╚══════╝╚══════╝╚══════╝   ╚═╝   ",
}

func runAskAgentTUI(c *client.Client, role, system string, maxTokens int, initialPrompt string) error {
	wd, _ := os.Getwd()

	st, _ := c.Status()
	if role == "" {
		for _, ep := range st.Endpoints {
			if strings.HasPrefix(ep.Name, "distributed-") {
				role = ep.Name
				break
			}
		}
		if role == "" {
			for _, r := range []string{"scout", "coder", "architect"} {
				if _, ok := st.Roles[r]; ok {
					role = r
					break
				}
			}
		}
		if role == "" && len(st.Endpoints) > 0 {
			role = st.Endpoints[0].Name
		}
		if role == "" {
			role = "scout"
		}
	}

	ctl := newChatCtl(c, role, system, runOnFromConfig)
	switchModelCmd := func(ctx context.Context, args []string, a *agenttui.App) (string, bool, error) {
		return ctl.modelCmd(ctx, args, a)
	}

	cfg := agenttui.Config{
		Title:          "fleet",
		Subtitle:       fmt.Sprintf("Cluster LLM Router · %s", c.Base),
		ModelName:      role,
		WorkDir:        wd,
		Mode:           "ask",
		HideMode:       true,
		Placeholder:    "Ask anything, or type '/' for commands (Ctrl+S to switch model) · Enter while answering queues, Ctrl+X sends now…",
		Theme:          agenttui.ThemeByName(os.Getenv("FLEET_THEME")),
		ShowHeader:     true,
		ShowFooter:     true,
		EnableMouse:    true,
		Banner:         fleetBanner,
		HeaderInfo:     ctl.header,
		HeaderInfoWide: ctl.header,
		InfoRows:       ctl.infoRows,
		FooterHint:     "Enter send (queues while answering) · Ctrl+X send now · Esc cancel · / commands · Ctrl+S models · Ctrl+C quit",
		QuickActions: []agenttui.QuickAction{
			{Cmd: "/model", Desc: "Switch model or role, manage models (Ctrl+S)"},
			{Cmd: "/status", Desc: "Cluster nodes and active endpoints"},
			{Cmd: "/top", Desc: "TPS leaderboard & performance"},
			{Cmd: "/clear", Desc: "Clear conversation turns"},
			{Cmd: "/theme", Desc: "Switch visual theme"},
		},
		OnKey: func(key string, a *agenttui.App) bool {
			if key == "ctrl+s" {
				a.Submit("/model")
				return true
			}
			return false
		},
		Commands: []agenttui.SlashCommand{
			{
				Name:        "model",
				Description: "Switch active model or role (words filter the list; \"manage\" opens model management)",
				Usage:       "[name | words | manage]",
				Handler:     switchModelCmd,
			},
			{
				Name:        "role",
				Description: "Alias for /model (switch active role or model)",
				Usage:       "[name]",
				Handler:     switchModelCmd,
			},
			{
				Name:        "top",
				Description: "Show tokens/sec leaderboard & response speeds",
				Handler: func(ctx context.Context, args []string, a *agenttui.App) (string, bool, error) {
					currSt, err := c.Status()
					if err != nil {
						return "", false, err
					}
					return "```\n" + renderBoard(currSt.Endpoints) + "\n```", true, nil
				},
			},
			{
				Name:        "status",
				Description: "Show fleet cluster status and endpoints",
				Handler: func(ctx context.Context, args []string, a *agenttui.App) (string, bool, error) {
					currSt, err := c.Status()
					if err != nil {
						return "", false, err
					}
					var b strings.Builder
					b.WriteString(fmt.Sprintf("**Fleet Cluster Status** (v%s)\n\n", currSt.Version))
					b.WriteString("Active Endpoints:\n")
					for _, ep := range currSt.Endpoints {
						status := "🟢 OK"
						if !ep.OK {
							status = "🔴 " + ep.Err
						}
						desc := ep.Model
						if ep.Group != nil && ep.Group.Engine != "" {
							desc = fmt.Sprintf("group %s (%d slots)", ep.Group.Engine, ep.Slots)
						}
						b.WriteString(fmt.Sprintf("- **%s** (`%s`) on `%s` — %s (%dms)\n", ep.Name, desc, ep.Node, status, ep.LatencyMS))
					}
					if len(currSt.Roles) > 0 {
						b.WriteString("\nConfigured Roles:\n")
						for r, x := range currSt.Roles {
							b.WriteString(fmt.Sprintf("- **%s** → `%s`\n", r, x.Active))
						}
					}
					return b.String(), true, nil
				},
			},
			{
				Name:        "theme",
				Description: "Switch visual theme",
				Usage:       "[name]",
				Handler: func(ctx context.Context, args []string, a *agenttui.App) (string, bool, error) {
					themes := agenttui.AvailableThemes()
					if len(args) > 0 {
						t := strings.TrimSpace(args[0])
						a.SetTheme(agenttui.ThemeByName(t))
						return fmt.Sprintf("Theme changed to **%s**", t), true, nil
					}
					choice, err := a.Ask(ctx, agenttui.Question{
						Text:    "Choose UI Theme:",
						Options: themes,
					})
					if err != nil {
						return "", false, err
					}
					a.SetTheme(agenttui.ThemeByName(choice))
					return fmt.Sprintf("Theme changed to **%s**", choice), true, nil
				},
			},
			{
				Name:        "clear",
				Description: "Clear conversation turns",
				Handler: func(ctx context.Context, args []string, a *agenttui.App) (string, bool, error) {
					a.ClearMessages()
					ctl.clear()
					return "Conversation cleared.", true, nil
				},
			},
		},
		Stream: ctl.stream,
	}

	app := agenttui.New(cfg)
	wctx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	go ctl.watch(wctx)
	if initialPrompt != "" {
		go func() {
			time.Sleep(150 * time.Millisecond)
			app.Submit(initialPrompt)
		}()
	}

	return app.Run(context.Background())
}
