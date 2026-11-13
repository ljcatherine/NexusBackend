# Security

Report vulnerabilities privately through GitHub's **Report a vulnerability** button on this
repository's Security tab, not in public issues, and never include credentials in a report.

This backend has not been independently audited. Its design rules:

| Area | Control |
| --- | --- |
| Exposure | The gateway binds to loopback only. It is reached through a Cloudflare Tunnel and Workers VPC; no inbound ports are opened. SSH, inference, RCON and panel ports stay private. |
| Authentication | Cloudflare Access in front of the Worker, an owner-email and audience check in the Worker, and independent OIDC verification plus per-subject permissions in the gateway. |
| Route surface | The Worker forwards fixed routes and methods only, with bounded bodies. Unknown paths are refused. |
| Panel isolation | With `PANEL_HOSTNAME` set, the Minecraft panel gets its own origin that serves nothing else, and Cloudflare's `CF_*` cookies are stripped before the panel sees a request. |
| SSH relay | Server-held Ed25519 key, strict `known_hosts`, fixed target aliases, and `restrict,pty` on upstream `authorized_keys`. |
| Commands and patches | Agent proposals need separate human review and a separate human execute step, plus explicit per-root grants. Empty grants deny everything. |
| Secrets | Real environment files, tunnel tokens, keys, Access audience tags and the live `wrangler.jsonc` are never committed; the repo ships `.example` templates only. The tunnel unit reads its token from a root-owned file, not process arguments. |
