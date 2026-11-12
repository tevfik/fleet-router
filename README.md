# fleet-router

One address for all your model servers.

You run models on a few machines: Ollama on the laptop, llama.cpp on a desktop, vLLM on a server.
`fleet` finds them, puts them behind one OpenAI- and Ollama-compatible endpoint, and sends each request
to a model that is up.

```
your apps ──► fleet (127.0.0.1:4711) ──► laptop   ollama
                                     ├─► desktop  llama-server   (over ssh)
                                     └─► server   vLLM           (over ssh)
```

## Install

Linux, amd64 or arm64:

```bash
curl -fsSL https://raw.githubusercontent.com/tevfik/fleet-router/main/install.sh | sh
```

Add `-s -- --service` to also install a systemd user service. From source: `go install github.com/tevfik/fleet-router/cmd/fleet@latest`

## First steps

```bash
fleet serve &              # start the router
fleet discover --apply     # find models on this machine and on your ssh hosts
fleet role init --apply    # make starter roles (scout, coder, …) from what was found
fleet ask scout "hello"    # try it
```

Run `fleet` with no arguments for the menu.

## Use it

Any OpenAI client works. The model name is a **role**:

```bash
curl http://127.0.0.1:4711/v1/chat/completions \
  -d '{"model": "scout", "messages": [{"role": "user", "content": "hi"}]}'
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:4711/v1", api_key="none")
client.chat.completions.create(model="coder", messages=[{"role": "user", "content": "hi"}])
```

Ollama tools work too: `OLLAMA_HOST=127.0.0.1:4711 ollama run scout`.

## Roles

A role is a name for a kind of job. Your apps ask for `coder`; you decide which model that is, and change it
any time without touching the apps:

```bash
fleet role set coder desktop-qwen2-5-coder laptop-llama3-2   # preferred, then fallback
```

If the preferred model is down, the next one answers. See [docs/roles.md](docs/roles.md).

## Configuration

Everything lives in `~/.config/fleet/config.yaml`. `fleet discover --apply` writes it for you; by hand it looks
like this:

```yaml
endpoints:
  - {name: laptop-llama3-2, node: laptop, url: http://127.0.0.1:11434/v1, model: "llama3.2"}
  - {name: desktop-qwen2-5-coder, node: desktop, ssh: desktop, remote: 127.0.0.1:11434, model: "qwen2.5-coder"}
roles:
  coder: {preferred: desktop-qwen2-5-coder, fallback: [laptop-llama3-2]}
```

`ssh: desktop` means: reach it through `ssh desktop` (fleet opens the tunnel). Changes apply without a restart.

## Common commands

| Command | What it does |
|---|---|
| `fleet` | menu |
| `fleet status` · `fleet doctor` | what is up, and what could break |
| `fleet discover` | look for model servers |
| `fleet role …` | set up roles |
| `fleet ask <role> "…"` | one question from the shell |
| `fleet nodes` | the machines: load, memory, GPUs |
| `fleet update` | install the newest release (the service also does this by itself) |

## More

- [docs/roles.md](docs/roles.md): roles, load balancing, images and embeddings
- [docs/machines.md](docs/machines.md): ssh, other networks (hub), other fleets, sharing with your LAN
- [docs/clients.md](docs/clients.md): Claude Code, OpenCode, Ollama API, model checks, logs

The source also contains a runner that splits one model over several machines. It only shows up when
fleet-engine is installed, which is not part of this repository.
