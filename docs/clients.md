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
fleet setup claude      # MCP server + a skill that tells Claude which role to use for what
fleet setup opencode    # a fleet provider and one agent per role
```

Claude can then hand work to your models (`fleet_ask`, `task_start`, …).

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
