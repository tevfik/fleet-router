# AGENTS.md

Notes for coding agents working on this repository.

## What it is

`fleet` is one Go binary: a router that puts model servers on many machines (Ollama, llama.cpp, vLLM, …)
behind one OpenAI- and Ollama-compatible endpoint, plus a CLI, a menu (TUI) and an MCP server.

## Layout

| Path | What |
|---|---|
| `cmd/fleet` | the CLI: `main.go` dispatches, one file per command group (`cmd_*.go`) |
| `internal/server` | the daemon: router (`/v1`, `/api`), admin API (`/fleet/*`), hub, peers |
| `internal/config` | `config.yaml`: schema, defaults, validation |
| `internal/tunnel` | ssh tunnels and the hub's reverse tunnel |
| `internal/tui` | the menu |
| `internal/discover`, `roles`, `chatpick`, `client`, `mcpsrv`, `setup`, `update`, `eval`, `hostinfo`, `mdns` | what the names say |
| `internal/feature` | `feature.Runner()`: whether the runner side is available |
| `internal/deploy`, `plan`, `store`, `profile`, `ringengine`, `pkg/stagemod` | the runner side (needs fleet-engine, hidden otherwise) |

## Build and test

```bash
make build                      # ./fleet
make check                      # go vet, all tests, gofmt
go test ./...                   # with the runner side
FLEET_RUNNER=0 go test ./...    # without it: both must pass
```

End-to-end against real model servers (CI starts two tiny llama.cpp servers; any OpenAI-compatible server works):

```bash
make build && FLEET_E2E_BIN=$PWD/fleet \
  FLEET_E2E_CHAT_URL=http://127.0.0.1:11434/v1 FLEET_E2E_CHAT_MODEL=llama3.2 \
  FLEET_E2E_SSH_HOST=localhost \
  go test -tags e2e -v ./test/e2e
```

## Rules

- Go only; prefer the standard library. Run `gofmt`.
- Every change comes with a test. Tests never reach real machines or the real config: use `httptest`, a temp
  `HOME` and `FLEET_CONFIG` (see the helpers in existing tests).
- Runner features (CLI help and commands, menu items, admin routes, background loops) are shown only when
  `feature.Runner()` is true.
- Config changes are applied hot through `Server.SetConfig`; keep `config.yaml` backward compatible.
- No real host names, addresses or keys anywhere: use `example.com`, `10.0.x.x`, names like `laptop`, `desktop`.
- User-facing text is English. Docs stay short: `README.md` and a few files in `docs/`.
- License: AGPL-3.0 ([LICENSE](LICENSE)); contributions are also licensable commercially ([CONTRIBUTING.md](CONTRIBUTING.md)).
