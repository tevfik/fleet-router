# Machines

fleet reaches models in three ways: on this machine, over ssh, and through other fleets.

## Over ssh

If `ssh desktop` works without a password, fleet can use the models on `desktop`:

```bash
fleet discover --ssh desktop --apply
```

fleet opens one ssh tunnel per machine (`ssh -L`), keeps it alive and closes it when idle. Nothing needs to
be installed on the other side. The tunnel listens on 127.0.0.1 only.

## Machines on another network (hub)

A machine behind NAT cannot be reached by ssh. It can still join: it opens a connection out to a **hub**.
Any `fleet serve` can be a hub; it only has to be reachable.

On the hub (a server everyone can reach):

```bash
fleet serve        # with listen: 0.0.0.0:4711 and an API key in the config, see below
```

On each machine that should join:

```bash
fleet join http://hub.example.com:4711
```

Its models then appear on the hub. `fleet join --leave` disconnects. Use `--insecure` for a hub with a
self-signed certificate.

## Other fleets (peers)

Run fleet on several machines and connect them; each one sees the models of the others as `machine/model`.

```bash
fleet peers lan                          # fleets heard on this network
fleet peers pair                         # on machine A: shows a 6-digit code
fleet peers add a http://10.0.0.5:4711/v1 pair=123456   # on machine B
```

## Share with your LAN

By default fleet only answers on 127.0.0.1. To let other machines use it:

```yaml
listen: 0.0.0.0:4711
api_key_env: FLEET_API_KEY     # the key itself goes into ~/.config/fleet/.env
```

Clients then send `Authorization: Bearer <key>`. `fleet apikey create` makes extra keys, limited to some roles
or models if you like.

## See the machines

`fleet nodes` (menu: **Nodes**) shows each machine's CPU, memory, GPUs and temperatures, and a score of how
big and how fast a model it can run. A machine that is short of memory or too hot gets fewer requests.
