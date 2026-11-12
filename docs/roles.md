# Roles

A role is a name for a kind of job (`scout`, `coder`, `vision`, `embed`, …). Apps put the role in the `model`
field; fleet picks a real model for it. You choose the role, fleet does not read the prompt.

## Set one up

```bash
fleet role set coder desktop-qwen        # one model
fleet role set coder desktop-qwen laptop-llama   # preferred, then fallback
fleet role test coder                    # who answers, how fast
fleet role list
```

Or in the menu: **Roles**.

## Spread the load

By default a role uses the first model that is up (`failover`). Other strategies:

| strategy | picks |
|---|---|
| `failover` | the first healthy model, in your order |
| `least-busy` | the model with the fewest requests running |
| `round-robin` | each model in turn |
| `fastest` | the model with the best measured speed |
| `cost` | the model expected to finish this request first |

```bash
fleet role opt coder strategy=least-busy
```

Instead of listing models, you can tag them and let the role pick them up:

```bash
fleet role tag desktop-qwen fast
fleet role match helper tag:fast
```

New models with the tag join the role by themselves. Other selectors: `node:desktop`, `cap:vision`, `model:qwen*`.

## Slots

`slots` is how many requests a model handles at once; the rest wait in a queue.

```bash
fleet role slots desktop-qwen 4
```

## Images, embeddings, long prompts

fleet checks what a request needs:

- a request with an image only goes to models that can see (`vision`);
- `/v1/embeddings` only goes to embedding models, and never mixes models with different vector sizes;
- a prompt too long for a model's context skips that model.

If no model fits, you get a clear error instead of a wrong answer. `fleet probe` tests what each model can
really do.

## In the config file

```yaml
roles:
  scout:  {preferred: laptop-llama, fallback: [desktop-qwen]}
  coder:  {preferred: desktop-qwen, strategy: least-busy}
  helper: {match: ["tag:fast"], strategy: fastest}
```
