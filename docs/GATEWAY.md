# Gateway and Cloudflare

Everything here is optional. Without a gateway, Nexus talks directly to the SSH
hosts and AI endpoints you configure, over your LAN or Tailscale.

The gateway path adds remote access without opening ports:

```
Nexus (iPhone)
  └─ HTTPS + Cloudflare Access (Managed OAuth, PKCE)
       └─ Cloudflare Worker  (cloudflare/worker.mjs, fixed routes only)
            └─ Workers VPC → cloudflared tunnel → gateway on 127.0.0.1:8787
                 ├─ AI upstream (NexusAI or another OpenAI-compatible server)
                 ├─ optional PS5 queue, status and media APIs (NexusAI control and media)
                 ├─ SSH relay to fixed aliases (server-held key, strict known_hosts)
                 ├─ optional web search (Brave or SearXNG)
                 ├─ optional encrypted conversation sync
                 └─ optional Minecraft panel on 127.0.0.1:8080
```

Workers VPC and Managed OAuth are Cloudflare betas; behaviour can change.

## Gateway (`gateway/`)

A Go service that only binds to loopback and refuses to start with missing policy.
The API contract is in [`gateway/openapi.yaml`](../gateway/openapi.yaml).

| Route | Permission | Notes |
|---|---|---|
| `GET /healthz` | none | Process health only |
| `GET /v1/models` | `ai.models` | Returns only IDs in `NEXUS_AI_ALLOWED_MODELS` |
| `POST /v1/chat/completions` | `ai.chat` | Bounded request subset, streaming, up to three PNG or JPEG images per user message; in PS5 mode requests wait in the PS5 queue. Tool calls limited to two read-only snapshot tools that the app answers after user approval |
| `POST /v1/chat/completions/input_tokens` | `ai.chat` | Token count from the upstream llama.cpp server |
| `POST /v1/search` | `ai.search` | Brave or SearXNG; disabled until configured |
| `POST /v1/research` | `ai.chat` + `ai.search` | Plan, search, fetch and answer in one request |
| `/v1/conversations…` | `ai.history` | AES-256-GCM encrypted per subject; disabled until a key is set |
| `GET /v1/ssh/{target}` | `ssh.connect` | WebSocket SSH relay to an alias in `NEXUS_SSH_TARGETS_JSON` |
| Panel hostname root | `minecraft.panel` | Fixed proxy to the panel on `127.0.0.1:8080`, only on `PANEL_HOSTNAME` |
| `GET /v1/ai/status` | `ai.models` | PS5 state, queue length and the caller's own jobs. Always registered; reports offline when PS5 mode is off |
| `GET /v1/ai/jobs/{id}` | `ai.models` | State of one of the caller's jobs |
| `GET /v1/ai/jobs/{id}/result` | `ai.models` | Video bytes, returned once. PS5 mode only |
| `POST /v1/audio/transcriptions` | `ai.audio` | WAV to text. PS5 mode only |
| `POST /v1/audio/speech` | `ai.audio` (+ `ai.voice.clone` for a reference voice) | Text to WAV. PS5 mode only |
| `POST /v1/voice/chat` | `ai.voice` (+ `ai.voice.clone` for a reference voice) | One spoken turn, streamed as SSE. PS5 mode only |
| `POST /v1/images/generations` | `ai.image` | PNG as JSON, or SSE with `"stream": true`. PS5 mode only |
| `POST /v1/videos/generations` | `ai.video` | Starts a job (202); fetch the result when it is done. PS5 mode only |

Media routes are registered only in PS5 mode and answer `404 not found` otherwise.

Security properties:

- JWT access tokens are verified against the issuer's JWKS (issuer, audience,
  signature, time). The verified principal must have the permission in
  `NEXUS_SUBJECT_PERMISSIONS_JSON`; the client can't grant itself anything. The policy
  parser rejects duplicate or ambiguous keys and unknown permissions.
- User tokens are never forwarded upstream. An optional upstream service token comes
  from `NEXUS_AI_UPSTREAM_TOKEN`.
- Upstream URLs can't contain credentials, queries or fragments. Cleartext HTTP is
  allowed only to private, link-local, loopback or Tailscale IPv4 addresses, and
  redirects aren't followed.
- Request size, message count, tokens, rate and concurrency are bounded. Logs and
  audit events never contain tokens, prompts, client IPs or conversation IDs.
- Search page fetches resolve DNS themselves, allow only public unicast addresses,
  pin the connection, cap size, and mark retrieved text as untrusted.

### Run locally

```sh
nix develop
mkdir -p local
cp gateway/.env.example local/nexus-gateway.env
chmod 600 local/nexus-gateway.env   # then replace every example value
./scripts/gateway-dev.sh
```

### Deploy

```sh
cd gateway
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go build -trimpath -o nexus-gateway .
```

Video generation needs `ffmpeg` on the gateway host's `PATH`, built with the `libx264`
(mp4) and `libvpx-vp9` (webm) encoders. Without it, video jobs end in state `failed`.

The service units `nexus-gateway.service` (system), `nexus-gateway-user.service`
(user) and `nexus-cloudflared.service` are templates; review them for your host. The
tunnel unit reads its token from a root-owned file with `--token-file` so the token
never appears in process arguments. Never expose port 8787, the inference port or
SSH directly.

## PS5 inference (optional)

Set both `NEXUS_PS5_CONTROL_URL` and `NEXUS_PS5_MEDIA_URL` to turn on PS5 mode. Chat and
media then share one queue in front of the NexusAI PS5 server. With neither set, chat goes
straight to `NEXUS_AI_UPSTREAM_URL`, the media routes are not registered, and
`GET /v1/ai/status` reports `online: false`, `reason: ps5_offline`.

In PS5 mode, `NEXUS_AI_UPSTREAM_URL` is the PS5 chat endpoint (llama-server, usually port
8081) and `NEXUS_AI_UPSTREAM_TOKEN` is sent with chat only. The PS5 bearer comes from the
systemd credential named by `NEXUS_PS5_TOKEN_CREDENTIAL` (`LoadCredential=`) and goes only to
the control API, the media API and the health probes. The gateway reads it again on each
request, so rotating the token needs no restart. Keep every PS5 address on a private
IPv4 network or behind HTTPS, and never expose them.

### Queue and scheduling

- One PS5 job runs at a time. A chat request or media job waits in the queue for its turn.
- A subject may have `NEXUS_AI_QUEUE_PER_SUBJECT` jobs waiting, and the queue holds at most
  `NEXUS_AI_QUEUE_GLOBAL`. Over either limit the request gets `429 PS5 queue is full` with
  `Retry-After: 10`.
- A job that has waited longer than `NEXUS_AI_MAX_QUEUE_WAIT` runs next, oldest first. Otherwise
  the scheduler prefers jobs for the PS5's current engine and model, then jobs for its current
  engine, then the oldest job. Within that group the subject served least recently goes first,
  so one subject's burst cannot run ahead of the others.
- Nothing is rejected for waiting. A queued request waits until it runs or the caller
  disconnects. The generation timeout starts only when the job gets the PS5.

### Health

The gateway polls the PS5 control status (and, in media mode, the media `/health`) every
`NEXUS_PS5_POLL_INTERVAL`. Three consecutive failed probes open a breaker: the PS5 is reported
offline with `reason: ps5_offline`, and one probe is retried after 30 seconds. Each change
between online and offline raises an alert. Reasons reported while the PS5 is online:
`switching`, `loading`, `no_model`, `failed`, `restart_failed`, `media_unavailable`.

### Chat in PS5 mode

The response is the chat endpoint's stream or JSON, unchanged. Failures before any output:

| Status | Body message | Notes |
|---|---|---|
| 503 | `PS5 is offline` | `Retry-After: 30`. Checked before the body is read. Also returned when the PS5 stays offline for 45 s during an engine switch |
| 429 | `PS5 queue is full` | `Retry-After: 10` |
| 503 | `PS5 could not switch to chat mode` | `Retry-After: 30`. The PS5 was on another engine and the switch to `llm` failed |
| 503 | `PS5 chat is not ready` | `Retry-After: 30`. Timed out waiting for the switched engine to report ready (`ps5EngineSwitchTimeout`, 10 min) |
| 503 | `AI inference or model management is busy`; `model state is unavailable or a switch is awaiting confirmation`; `orchestrator task state is unavailable`; `an orchestrator task is using PS5 inference` | The lifecycle gate shared with model switches and orchestrator runs |
| 502 | `AI inference service unavailable`; `AI inference request failed` | The chat endpoint failed |

A chat that finds the PS5 on another engine waits in the queue while the gateway switches it to `llm` and
waits for it to load, then streams with `200`.

**Needs Verification:** chat and image requests can stay open for about 100 s during a model load or about
30 s during an app restart, and longer up to the 10-minute switch timeout. If the gateway is reached through
Cloudflare, Cloudflare's default origin response timeout (about 100 s on non-Enterprise plans; confirm for your
plan) may cut such a request off with a `524`. Video is unaffected because it runs as an async job.

Images in `messages`: a `user` message may hold a text part first, then one to three
`image_url` parts. Each image must be a PNG (`data:image/png;base64,`) or JPEG
(`data:image/jpeg;base64,`) data URL with strict base64 and at most 4 MiB decoded. All images
together may decode to at most 4 MiB. The bytes must match the declared type: a PNG signature,
or a JPEG that starts `FF D8 FF` and ends `FF D9`. The data URL is at most 5,592,431 characters.
Images on any other role are rejected with `images are only allowed on user messages`.

### Media routes

Valid requests are checked in this order: route permission (`403 permission denied`), body
(`400`, `413`), PS5 online (`503 PS5 is offline`), daily quota, queue (`429 PS5 queue is full`),
lifecycle gate, engine switch, run. Validation runs before anything is queued, so an invalid
request uses no quota and no PS5 time. Media routes do not use the `NEXUS_AI_REQUESTS_PER_MINUTE`
budget; the queue and quotas limit them instead.

| Route | Body cap | Input rules | Success |
|---|---|---|---|
| `POST /v1/audio/transcriptions` | 16 MiB | `audio_b64`: standard base64 WAV, at most 10 MiB decoded and 60 s. Optional `language` (letters and hyphens, at most 16) and `translate`. `model` is ignored; the server fixes it | `200` `application/json`: `{"text","language","segments":[{"start","end","text"}]}`, `Cache-Control: no-store` |
| `POST /v1/audio/speech` | 4 MiB | `input`: 1 to 1000 characters. Optional `language`. Optional `voice_reference_b64` (WAV, at most 2 MiB) with `consent: true`, which needs `ai.voice.clone`. The model is fixed | `200` `audio/wav`, `Cache-Control: no-store` |
| `POST /v1/voice/chat` | 16 MiB | `audio_b64` as above. Optional `messages` of `{role: user or assistant, content}`; only the newest 20 turns and at most 16 KiB of content are kept. The system prompt is fixed and any client value is ignored. Optional `speaker_b64` and `consent`, which need `ai.voice.clone` | `200` `text/event-stream` (see below) |
| `POST /v1/images/generations` | 12 MiB | `prompt`: 1 to 2000 characters. Optional `model`: `sd-turbo-fp16` or `flux2-klein-4b` (empty uses the PS5 default). `width` and `height`: multiples of 64 from 256 to 1024. Optional `steps` (1 to 100) and `seed` (-1 or more). Optional `init_image_b64` and up to four `ref_images_b64`, each PNG or JPEG of at most 8 MiB. Optional `stream` | `200` `application/json`: `{"data":[{"b64_json":"<PNG>"}]}`, or SSE with `stream: true` |
| `POST /v1/videos/generations` | 16 MiB | `prompt`: 1 to 2000 characters. Optional `model`: `wan2.1-t2v-1.3b` (the default). `width` and `height`: multiples of 16 from 128 to 1280. `frames`: 4k+1, at most 33. Optional `init_image_b64` (at most 8 MiB). Optional `format`: `mp4` (default) or `webm` | `202` `{"id":"<32 hex>"}` |

Errors common to the media routes:

| Status | Body message | When |
|---|---|---|
| 503 | `PS5 is offline` | `Retry-After: 30`; checked before quota, so no quota is charged |
| 403 | `no daily quota is configured for this AI permission` | No rule in `NEXUS_AI_QUOTAS_JSON` for the permission |
| 429 | `daily quota exceeded` | The subject has used the day's limit |
| 429 | `PS5 queue is full` | `Retry-After: 10` |
| 503 | `AI inference or model management is busy` (and the other lifecycle messages above) | The lifecycle gate is held |
| 503 | `PS5 could not switch to media mode`; `PS5 media is not ready` | `Retry-After: 30`. Readiness is waited for up to 10 minutes |
| 502 | A sanitized PS5 message, such as `PS5 unreachable`, `PS5 request timed out`, `PS5 returned an invalid image`, `PS5 returned an invalid response`, `PS5 video response was invalid`, `PS5 returned no WAV audio`, `PS5 response too large`, or `AI media request failed` | The PS5 failed or returned invalid output |

Route-specific validation errors are `400` with a message naming the field, for example
`prompt exceeds 2000 characters`, `model is not an allowed model`, `frames must be 4k+1 and at most 33`,
`format must be mp4 or webm`, `audio_b64 exceeds 60 seconds` or `audio_b64 must be a WAV file`.
Bodies over the cap get `413 request body is too large` (transcription and speech), `413 voice request is too large`
(voice chat), or `400` with `invalid image request` or `invalid video request`.
Reference voices: speech without `ai.voice.clone` gets `403 voice cloning requires the ai.voice.clone permission`,
and speech without `consent: true` gets `403 consent is required for voice cloning`. Voice chat checks consent first, so a
reference voice without consent gets `400 consent is required for voice cloning`, and without `ai.voice.clone` gets `403 permission denied`.

Run time limits per route: image 10 minutes, video 60 minutes, transcription 2 minutes,
speech 5 minutes, voice 3 minutes. The engine switch and readiness wait are 10 minutes each.

**Image.** Without `stream`, the reply is the first PNG in one JSON response. With `stream: true`
the reply is SSE: each PS5 event is relayed as one `data:` line, and a successful run ends with
`data: [DONE]`. A refusal before the first event is a plain JSON error. A failure after that ends
with `data: {"error":{"message":"..."}}`, unless the PS5 already sent an error event; `[DONE]` is
sent only on success.

**Voice chat.** The gateway relays each PS5 event as one `data:` line without parsing it. The
event names come from the PS5 plan (`status`, `transcript`, `text`, `audio` with `audio_b64`,
`done`), and a PS5 error arrives as `{"type":"error","message":"..."}`. Once the stream has
started, the gateway always ends it with `data: [DONE]`. If the PS5 stream breaks without an
error event, the client sees `[DONE]` alone.

**Video.** The job runs under the server's lifetime, not the request's, so it survives the
client disconnecting. Poll `GET /v1/ai/jobs/{id}` until the state is `done`, then fetch
`GET /v1/ai/jobs/{id}/result` once. The gateway encodes the PS5's PNG frames with ffmpeg. There
is no cancel route. A failed job ends in state `failed` and is not refunded. A finished result
stays for 10 minutes or until it is read, within a shared store of 128 MiB. If a result does not
fit, the job still reports `done` but its result route returns `404`.

Only video returns a job ID at submission. Image, speech and transcription run synchronously,
and their jobs appear in `GET /v1/ai/status` only while queued or running.

### Status and job routes

`GET /v1/ai/status` (`ai.models`) returns `online`, `engine`, `model`, `ready`, `busy` (true
while generating), `reason`, `queue_length` (all subjects), and `jobs`, which lists only the
caller's queued and running jobs. A queued job has a 1-based `position`; a running job has `0`.
If the caller's oldest queued job needs an engine or model switch, `warning` says so:
`Switching engine restarts the PS5 server and takes about 30 s.` or `Loading this model takes about 100 s.`

`GET /v1/ai/jobs/{id}` (`ai.models`) returns `id`, `state`, `position`, `engine` (`llm` or
`media`) and `model`. States are `queued`, `switching`, `loading`, `generating`, `done`,
`failed` and `cancelled`. An unknown, expired or other subject's ID gets the same `404 job not
found`. The process keeps its 256 most recent finished jobs.

### Quotas

`NEXUS_AI_QUOTAS_JSON` holds daily limits per permission, for example
`{"ai.video":{"unit":"requests","limit":5},"ai.voice":{"unit":"seconds","limit":900}}`.

- **Default deny.** A media permission with no rule is refused, even if it is granted.
- **Limit 0** refuses every use of that permission with `429 daily quota exceeded`.
- **Day.** Usage resets at UTC midnight and is counted per subject and permission.
- **Amounts** are charged at admission: 1 per image, video or speech request; whole seconds of
  audio, rounded up, for transcription and voice chat. `ai.audio` covers transcription (seconds)
  and speech (1 per request), so choose its limit with both in mind. The unit label is informational;
  the gateway counts the numbers.
- **Refunds.** The charge is returned when the PS5 never ran the job: the queue is full, the
  caller cancels before the job gets the PS5, the lifecycle gate refuses it, the engine switch
  fails or media is not ready, or the caller cancels before generation starts. Nothing is refunded
  once generation has started, even if it then fails. A refund after the UTC day has rolled over is
  skipped.

Each quota refusal raises a `quota_hit` alert.

### Usage log

With PS5 mode on, each job that reaches the PS5 slot writes one JSON line, including failed and
cancelled jobs. Requests refused before queueing, and requests cancelled while queued, write nothing.
Lines go to `usage-YYYY-MM.jsonl` (UTC month) in `NEXUS_USAGE_DIR`. The directory is created 0700
and the files 0600.

```json
{"ts":"2026-11-17T09:00:00Z","subject_hash":"<16 hex characters>","route":"/v1/images/generations","engine":"media","model":"sd-turbo-fp16","outcome":"done","queue_ms":1200,"run_ms":18000,"in_bytes":512,"out_bytes":88214}
```

`subject_hash` is the first 8 bytes of the SHA-256 of the OIDC subject, in hex. `outcome` is the
job's final state: `done`, `failed` or `cancelled`. Lines never hold prompts, chat messages, images,
audio, outputs, tokens or client IPs. `in_bytes` and `out_bytes` are sizes only.

### Alerts

Every alert is logged as `alert_event kind=<kind> count=<n>`. If `NEXUS_ALERT_WEBHOOK_URL` is set,
the alert is also POSTed as JSON: `{"kind","count","timestamp","message"}`, with fixed message text
and no request data. Posts time out after 5 seconds, redirects are not followed, at most four run at
once (an alert with no free slot is logged only), and each kind is sent at most once per 5 minutes.

| Kind | Fires when |
|---|---|
| `auth_failures` | Failed authentications in one UTC minute exceed `NEXUS_ALERT_AUTH_FAILURES_PER_MINUTE` |
| `request_spike` | All requests in one UTC minute exceed `NEXUS_ALERT_REQUESTS_PER_MINUTE` |
| `quota_hit` | A media quota is exhausted |
| `ps5_offline` / `ps5_online` | The PS5 changes state. The watcher assumes online before its first poll, so an offline first poll after gateway start raises `ps5_offline`; a healthy first poll raises nothing |
| `restart_failed` | The PS5 reports a failed restart |

### Permissions for media

| Permission | Grants |
|---|---|
| `ai.image` | `POST /v1/images/generations` |
| `ai.video` | `POST /v1/videos/generations` (the result and status routes need `ai.models`) |
| `ai.audio` | `POST /v1/audio/transcriptions`, `POST /v1/audio/speech` |
| `ai.voice` | `POST /v1/voice/chat` |
| `ai.voice.clone` | A reference voice on speech or voice chat, in addition to `ai.audio` or `ai.voice` |

Grant them in `NEXUS_SUBJECT_PERMISSIONS_JSON`. The policy parser rejects names not listed here.

### PS5 environment variables

Set the two URLs together, or neither. Integers outside their range stop startup; a non-numeric
integer falls back to its default. Durations that do not parse stop startup.

| Variable | Default | Accepted values |
|---|---|---|
| `NEXUS_PS5_CONTROL_URL` | empty (PS5 mode off) | `http://` on a private IPv4 address, or `https://`. No path, credentials, query or fragment. Set with `NEXUS_PS5_MEDIA_URL` |
| `NEXUS_PS5_MEDIA_URL` | empty | Same rules as the control URL |
| `NEXUS_PS5_TOKEN_CREDENTIAL` | none; required with the URLs | Name of a systemd credential: letters, digits, `.`, `-`, `_`. The file is a regular file of at most 4096 bytes holding at least 32 printable ASCII characters with no whitespace, and it must not be group or other readable (non-Windows). Loaded at startup, then per request |
| `NEXUS_PS5_POLL_INTERVAL` | `5s` | `1s` to `5m` |
| `NEXUS_AI_QUEUE_PER_SUBJECT` | `3` | 1 to 20 |
| `NEXUS_AI_QUEUE_GLOBAL` | `20` | From `NEXUS_AI_QUEUE_PER_SUBJECT` to 200 |
| `NEXUS_AI_MAX_QUEUE_WAIT` | `120s` | `10s` to `30m`. Sets scheduling priority only; it never rejects a job |
| `NEXUS_AI_QUOTAS_JSON` | empty (no quotas, so media is refused) | JSON object keyed by the media permissions. Each rule is `{"unit":"requests" or "seconds","limit":N}`, N at least 0. Unknown keys, units or negative limits stop startup |
| `NEXUS_USAGE_DIR` | `/var/lib/nexus-gateway/usage` | Absolute, normalized path. Created 0700 |
| `NEXUS_ALERT_WEBHOOK_URL` | empty (log only) | Clean `https://` URL: no credentials, query or fragment. Never echoed in errors |
| `NEXUS_ALERT_AUTH_FAILURES_PER_MINUTE` | `20` | 1 to 10000 |
| `NEXUS_ALERT_REQUESTS_PER_MINUTE` | `600` | 1 to 100000 |

Example values (replace them with your private addresses, kept out of Git):
`NEXUS_PS5_CONTROL_URL=http://10.0.0.20:8082`, `NEXUS_PS5_MEDIA_URL=http://10.0.0.20:8083`.

## Cloudflare Worker (`cloudflare/`)

The Worker allows only fixed AI routes, fixed SSH aliases and the panel root. It
fails closed unless Cloudflare Access supplies the configured audience and owner
email, and it forwards only Cloudflare's signed `Cf-Access-Jwt-Assertion` to the
gateway (never the OAuth bearer or Access cookie). The gateway verifies that
assertion independently.

### Setup

1. Create the Worker and a Workers VPC service that targets the gateway's loopback
   listener through `cloudflared` 2025.7.0 or newer (QUIC, outbound UDP 7844).
2. Copy `cloudflare/wrangler.example.jsonc` to the ignored `cloudflare/wrangler.jsonc`
   and fill in the VPC `service_id`. Keep `preview_urls` off. Set `keep_vars: true`
   if you manage runtime variables in the dashboard.
3. Protect the Worker hostname with a **hostname-based** Access application (not
   Worker-level Access, which rejects WebSocket upgrades). Use an exact **Emails**
   policy for your address, enable Managed OAuth, and add
   `https://<your-worker>.workers.dev/oauth2redirect` as an allowed redirect URI.
4. Register the public PKCE client:

   ```sh
   python3 scripts/register-cloudflare-access-client.py \
     https://<your-team>.cloudflareaccess.com \
     --resource-url https://<your-worker>.workers.dev
   ```

5. Add runtime variables (**Settings → Bindings → Variables and Secrets**, not build
   variables): `ACCESS_AUDIENCE` (the Access application's AUD tag) and
   `OWNER_EMAIL` (lowercase). The Worker returns 403 until both are set.
6. Configure the gateway: `NEXUS_OIDC_ISSUER` = team issuer, `NEXUS_OIDC_AUDIENCE` =
   AUD tag, `NEXUS_OIDC_JWKS_URL` = `https://<team>.cloudflareaccess.com/cdn-cgi/access/certs`,
   `NEXUS_OIDC_PRINCIPAL_CLAIM=email`, and grant your email the permissions you need.
7. Put the Worker host, team issuer and client ID in your
   [deployment plist](https://github.com/ljcatherine/NexusiOS/blob/main/docs/CONFIGURATION.md), or enter them in the app under
   Settings → API Connections.

No client secret or service token is embedded in the app or repository.

### Diagnosing 403s

Callers always see a generic `access denied`. The Worker logs safe booleans to
Worker Observability: whether variables are set, whether `ctx.access` exists and
matches, and whether the identity email matched. It never logs emails, audiences,
JWTs, cookies, codes or state.

## SSH relay

Relay profiles have no private key on the phone. The gateway terminates SSH over
the authenticated WebSocket and opens a second SSH connection with a server-held
Ed25519 key and strict `known_hosts`. Keep the upstream key, the relay host key and
`known_hosts` owner-only (0600), and authorize the upstream public key on each
target with `restrict,pty`. Nexus still pins the relay's SSH host key separately
from TLS.

## Minecraft panel

The panel stays bound to loopback with its own login, session cookie and CSRF
checks. Give it its own hostname so script running in the panel cannot reach the AI,
workspace or SSH routes from the same origin:

1. Add a custom domain for the panel (for example `minecraft.example.com`) to the
   Worker and set `PANEL_HOSTNAME` to it. That hostname then serves only the panel,
   and the Worker's other hostnames stop serving it. Without `PANEL_HOSTNAME` the
   panel is served at the root of every Worker hostname (legacy behaviour).
2. Make sure the Worker's Cloudflare Access application covers that hostname.
3. On the gateway, set `NEXUS_MINECRAFT_PANEL_ORIGIN` (the panel's own configured
   origin) and `NEXUS_MINECRAFT_PUBLIC_ORIGIN` (`https://` plus the panel hostname),
   and grant `minecraft.panel`.

The Worker strips Cloudflare's `CF_*` cookies before forwarding, so the panel never
sees the Access token.

## Other services

- [`agent-orchestrator/`](../agent-orchestrator) runs agent tasks next to the gateway.
- [`workspace-agent/`](../workspace-agent) is a read-only workspace bridge per host.
- [NexusAI](https://github.com/ljcatherine/NexusAI) is the PS5 inference server.

Each has its own README.
