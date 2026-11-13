# NexusBackend

The self-hosted backend behind the Nexus apps ([NexusiOS](https://github.com/ljcatherine/NexusiOS),
[Nexus Online](https://github.com/ljcatherine/NexusOnline)): an authenticated AI and SSH gateway,
the Cloudflare Worker in front of it, an agent orchestrator, and a read-only workspace bridge.

```
Nexus apps
  └─ HTTPS + Cloudflare Access (owner only)
       └─ Cloudflare Worker  (cloudflare/, fixed routes only)
            └─ Workers VPC → cloudflared tunnel → gateway on 127.0.0.1:8787
                 ├─ AI upstream (NexusAI or another OpenAI-compatible server)
                 ├─ SSH relay to fixed aliases (server-held key, strict known_hosts)
                 ├─ agent orchestrator + workspace agents
                 ├─ optional web search and encrypted conversation sync
                 └─ optional Minecraft panel, on its own Access-protected hostname
```

Nothing listens publicly. The gateway binds to loopback and is reached only through the tunnel.

| Path | What it is |
|---|---|
| [`gateway/`](gateway) | Go gateway: OIDC-verified routes, AI proxy, SSH relay, panel proxy. Unit templates for systemd. |
| [`cloudflare/`](cloudflare) | Cloudflare Worker that checks Access, then forwards fixed routes over Workers VPC. |
| [`agent-orchestrator/`](agent-orchestrator) | Python service that runs reviewed agent tasks next to the gateway. |
| [`workspace-agent/`](workspace-agent) | Go read-only workspace bridge, one per host. |
| [`integrations/hermes/`](integrations/hermes) | Hermes engine worker used by the orchestrator. |
| [`scripts/`](scripts) | Local dev runner, Debian hardening and firewall rules, and their tests. |
| [`docs/`](docs) | [Gateway and Cloudflare setup](docs/GATEWAY.md), [restricted commands](docs/integrations/RESTRICTED-COMMANDS.md). |

## Setup

Start with [docs/GATEWAY.md](docs/GATEWAY.md). In short:

1. Copy `gateway/.env.example` to an owner-only file outside the repo (for example
   `~/.config/nexus-gateway/nexus.env`, mode 0600) and fill in your own values.
2. Build and test the gateway:
   ```sh
   cd gateway
   CGO_ENABLED=0 go test ./...
   CGO_ENABLED=0 go build -trimpath -o nexus-gateway .
   ```
3. Review and install the systemd unit templates in `gateway/`.
4. Copy `cloudflare/wrangler.example.jsonc` to the ignored `cloudflare/wrangler.jsonc`,
   fill in your Access audience, owner email and VPC service, then `npx wrangler deploy`.

## Tests

```sh
(cd gateway && CGO_ENABLED=0 go test ./...)
(cd workspace-agent && CGO_ENABLED=0 go test ./...)
(cd cloudflare && node --test)
(cd agent-orchestrator && python3 -m unittest)   # Linux only (uses fcntl)
python3 -m unittest discover -s scripts -p 'test_*.py'
```

## Secrets

This repository holds code and templates only. Real environment files, tunnel tokens,
SSH keys, Access audience tags and the live `wrangler.jsonc` stay on the server and are
ignored by Git. See [SECURITY.md](SECURITY.md) to report a vulnerability.
