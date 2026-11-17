# Clients

## OpenAI API

Base URL `http://127.0.0.1:4711/v1`, model = a role, an endpoint name, or `machine/model`.
`GET /v1/models` lists them. Chat, completions and embeddings are supported, streaming included.

## Ollama API

The same port answers Ollama's API, so Ollama tools work unchanged:

```bash
OLLAMA_HOST=127.0.0.1:4711 ollama run scout
```

Chat, generate, embeddings, tags and show are supported. Pulling or deleting models is not: fleet routes the
models you run, it does not manage them.

## Claude Code and OpenCode

```bash
fleet setup claude      # MCP server, a skill, and /fleet-review, /fleet-implement, /fleet-qa
fleet setup opencode    # the fleet provider, one agent per role, and the same three commands
fleet setup skills      # refresh the guidance after you change roles
```

Both are optional, per user (not run by `fleet setup all --system`), and need `claude` or `opencode` in `PATH`.

**Claude Code** gets `fleet mcp` registered as an MCP server (`claude mcp add --scope user fleet -- fleet mcp`)
and `~/.claude/skills/fleet/SKILL.md`. The skill's "which role for which job" table is generated from your live
configuration (preferred model, machine, context size, tokens/s, and `use:` of each role). Claude does not
analyse the prompt for you: it picks the role.

| MCP tool | |
|---|---|
| `fleet_status` | endpoint health, and which endpoint each role uses now |
| `fleet_ask` | one question to a role, answered at once (no tools); falls back if the endpoint is down |
| `task_start`, `task_result`, `task_list`, `task_cancel` | background jobs; start each task separately to run them in parallel |
| `fleet_role_set` | point a role at another endpoint (applied hot) |
| `fleet_discover` | find OpenAI-compatible servers on this machine and over ssh |
| `fleet_leaderboard` | tokens/second ranking, optionally measured first |

`task_start` has two modes. `chat` is one request through the router, no tools. `opencode` (the default) runs
`opencode run --agent fleet-<role>` in the given directory, so the model can read and edit files; it needs
OpenCode and `fleet setup opencode`. `model: fleet/<endpoint>` pins one endpoint instead of the role's choice.

**OpenCode** gets `providers.fleet` in `~/.config/opencode/opencode.json` (the old file is saved as `.bak.<time>`;
`fleet opencode` prints the provider without writing it) and the agents `fleet-scout`, `-architect`, `-coder`,
`-verifier`, `-tester`, `-validator`, `-hunter` (`model: fleet/<role>`). Files are named `fleet-*`, so they never
collide with your own; a hand-written file with the same name is saved first.

Limits:

- Claude cannot be *served as a model*: fleet has no Anthropic `/v1/messages` endpoint, so Claude Code itself
  cannot run on your models. It uses them through the tools above.
- Jobs live in the memory of the MCP process and are gone when Claude Code closes it.
- Output of a job is clipped when it is very long.

## From the shell

```bash
fleet ask coder "explain this" -f main.go
git diff | fleet ask scout "summarise this diff"
```

## Check model quality

```bash
fleet eval --role scout
```

24 short tasks with checks done by code (format, simple maths, language, extraction). Good for spotting a
broken or weak model, not for ranking strong ones.

## See what happens

| | |
|---|---|
| `fleet log -f` | requests as they happen: who answered, how fast |
| `fleet usage` | who used what, per client, role or day |
| `fleet top` | models by speed |
| `/metrics` | Prometheus metrics |
