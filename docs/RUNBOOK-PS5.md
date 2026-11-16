# PS5 backend runbook

Operator guide for the PS5 inference backend behind the Nexus gateway. It covers normal operation, health
checks, alert responses, token rotation, crash recovery, queue and quota settings, adding a model and the
usage log.

Placeholders: `<ps5-tailnet-ip>` is the PS5's Tailscale address, `<gateway-host>` is the machine that runs
`nexus-gateway`, and `<token-file>` is a token file path. Never paste real addresses, e-mail addresses or
tokens into this file, into tickets or into git.

**Needs Verification (NV)** marks anything this runbook could not confirm from the repositories. Check
each NV item on the real host before relying on it.

Sources: NexusBackend `gateway/` and `docs/GATEWAY.md`, `docs/tailscale-acl.example.hujson`; NexusAI
`README.md`, `docs/TESTING.md`, `native/nexus_control.cpp`, `native/nexus_media*.cpp`,
`patches/0006-nexus-provisioning-pin.patch`, `patches/0011-nexus-media-mode.patch`.

## 1. Overview

```
client (Nexus iOS / web)
  └─ HTTPS + Cloudflare Access ─ Worker ─ tunnel ─ <gateway-host>: nexus-gateway on 127.0.0.1:8787
                                                    │  (only path to the PS5; bearer in systemd credential)
                                                    └─ Tailscale, ACL: tag:nexus-gateway → tag:nexus-ps5
                                                         ├─ :8081  chat (llama-server, no API key)
                                                         ├─ :8082  NexusAI control API (bearer required)
                                                         └─ :8083  media API (image, video, STT, TTS, voice)
```

Rules that never change:

- The PS5 is never exposed publicly. Ports 8081 to 8083 are reachable only through the tailnet, and the
  ACL allows them only from the gateway tag (`docs/tailscale-acl.example.hujson`). The example file's
  own test (`scripts/test_tailscale_acl.py`) checks that exactly one rule reaches the PS5 and that it
  lists exactly 8081, 8082 and 8083.
- `:8081` has no API key (NexusAI plan, `model_plan.cpp:375-380`), so the ACL is its only protection.
- `:8082` and `:8083` are plain HTTP. `:8082` requires the bearer; `:8083` checks the peer address (loopback,
  Tailscale CGNAT range, and RFC 1918 only when `nexus-control.lan` exists) and uses the same token file.
- The gateway listens only on loopback. Cloudflare reaches it through the tunnel. Never open 8787, 8081,
  8082 or 8083 to the internet.

## 2. Start and stop

### 2.1 Gateway

System unit (`gateway/nexus-gateway.service`):

| Item | Value |
|---|---|
| User | `nexus-gateway` |
| Environment | `/etc/nexus-gateway/nexus.env` (from `gateway/.env.example`, mode 0600) |
| Bearer | `LoadCredential=nexus-ps5-token:/etc/nexus-gateway/ps5-token`, read at `$CREDENTIALS_DIRECTORY/nexus-ps5-token` |
| Binary | `/usr/local/bin/nexus-gateway` |
| State | `/var/lib/nexus-gateway` (model-state journal, usage log, conversations) |

```sh
sudo systemctl start nexus-gateway
sudo systemctl stop nexus-gateway
sudo systemctl restart nexus-gateway
systemctl status nexus-gateway --no-pager
journalctl -u nexus-gateway -n 200 --no-pager
```

User unit (`gateway/nexus-gateway-user.service`) uses `~/.config/nexus-gateway/nexus.env` and
`~/.config/nexus-gateway/ps5-token`, and `systemctl --user` instead of `sudo systemctl`. Start at boot
without a login needs `loginctl enable-linger <user>` (NV: confirm on the host).

Behaviour to know before a restart:

- Startup reads the bearer once (`gateway/main.go:332-338`). A missing or unreadable credential stops the
  service, and `Restart=on-failure` retries every 5 s.
- A pending model switch is kept in `NEXUS_MODEL_STATE_PATH` and reconciled on start
  (`gateway/main.go:767`). A restart does not replay inference.
- Restarting the gateway does not restart the PS5. Queued work on the gateway is lost; running PS5
  work finishes or is cut by its own timeout (NV).

Check the unit before enabling a change: `scripts/test_gateway_unit.py` asserts the `LoadCredential` lines
and `NEXUS_PS5_TOKEN_CREDENTIAL=nexus-ps5-token`.

### 2.2 Console (PS5 title)

- Start: launch the NexusAI title from the PS5 with the payload chain you use (NV: launch method is not
  in either repository; it depends on the console setup).
- Quit: patch 0011 mentions a quit request from the PC, `scripts/ps5lm-app.sh close`. That script is not in
  the NexusAI `scripts/` folder (NV: confirm where it lives). Quitting stops the media API and frees GPU
  memory (`stop_media`, `patches/0011`).
- The controller reads `nexus-control.token` once at launch (`native/nexus_control.cpp:211`). Any token
  change needs a relaunch (see section 6).
- If `nexus-control.token` is missing or invalid, the controller stays off, so `:8082` does not listen.
  The gateway then reports `ps5_offline` (see section 3).
- If `nexus-control.lan` is absent, `:8082` and `:8083` bind to loopback only and the gateway cannot
  reach them. Use the marker only when the ACL and firewall are in place.

### 2.3 Planned maintenance order

1. `GET /v1/ai/status` (client with access): confirm `queue_length` is 0, or accept that queued work fails.
2. Stop new work at the source (Nexus client) if you can.
3. Stop or relaunch the console title only after the queue is empty (`jobs` is empty).
4. Restart the gateway only when it needs a config or binary change.
5. Verify with section 3.

### 2.4 Media mode versus LLM mode

| Mode | Engine | Port | Serves | Selected by |
|---|---|---|---|---|
| `llm` | llama-server | 8081 (chat), controller on 8082 | `POST /v1/chat/completions` and the `input_tokens` count | Default. `POST /v1/nexus/engine/llm` |
| `media` | Nexus media server | 8083 | `POST /v1/images/generations`, `POST /v1/videos/generations`, `POST /v1/audio/transcriptions`, `POST /v1/audio/speech`, `POST /v1/voice/chat` | `POST /v1/nexus/engine/media` |

How the switch works:

- The gateway switches engines itself. A chat request calls `EnsureEngine("llm")` (`gateway/main.go:1350`)
  and a media job calls `EnsureEngine("media")` through `mediaReachEngine` (`gateway/media_runner.go:179`).
  Both send `POST /v1/nexus/engine/<engine>` to `:8082`.
- **There is no gateway route for an operator to switch engines.** Earlier notes that suggested
  switching "via the control API through the gateway" are wrong. Switching is automatic when a request
  of the other kind is admitted. For a manual switch, run the call from `<gateway-host>` directly against
  `:8082` with the bearer (never paste the token into a shell history).
- The controller answers `202` with `restart_required: true`, and the app restarts into the new engine.
  Poll `GET /v1/ai/status` until `engine` matches and `ready` is true. A switch request while one is
  pending, or while inference is active, returns `409` (`native/nexus_control.cpp:535-544`).
- Duration: the gateway warns that an engine switch takes about 30 s and a model load about 100 s
  (`gateway/ps5_scheduler.go:58-66`). The gateway gives a switch up to 10 minutes
  (`ps5EngineSwitchTimeout`).
- Media mode is selected on the console by `model.txt`: line 1 `media`, line 2 the LLM to return to
  (`patches/0011`). The file survives a relaunch.
- Chat requests during a switch to llm wait in the queue while the PS5 switches and loads (up to the
  10-minute switch timeout), then stream with `200`. Failures during the wait are listed in section 3 below.
  Media jobs wait in the queue and show `switching` or `loading`.
- **Needs Verification:** a chat or image request can stay open for about 100 s during a model load or about
  30 s during an app restart. Through Cloudflare, the default origin response timeout (about 100 s on
  non-Enterprise plans; confirm for your plan) may cut it off with a `524`. Video is unaffected (async job).

## 3. Health checks

### 3.1 `GET /v1/ai/status`

Authenticated (`ai.models` permission, through the gateway). Fields, from `gateway/ai_status.go`:

| Field | Meaning |
|---|---|
| `online` | The last poll reached the control API with a valid status. `false` when the breaker is open. |
| `engine` | `llm` or `media`, as reported by the controller. |
| `model` | Human label of the loaded model. |
| `ready` | `true` when the engine can take work. In media mode it also needs `:8083/health` to report ok. False while switching or after a restart failure. |
| `busy` | `true` while the controller is `generating`. |
| `reason` | Why the PS5 cannot serve now. Empty when the PS5 is ready. Values below. |
| `queue_length` | Jobs waiting for the PS5. |
| `jobs[]` | The caller's own jobs only: `id`, `state` (`queued`, `switching`, `loading`, `generating`, `done`, `failed`, `cancelled`), `position`. |
| `warning` | Set when the oldest queued job needs a switch, for example an engine switch of about 30 s. |

Poll timing: the poller runs every `NEXUS_PS5_POLL_INTERVAL` (default 5 s). Each probe times out after 3 s.
The breaker opens after 3 failed probes in a row and allows one half-open probe after 30 s. Recovery
is therefore seen within about 30 s of the PS5 returning.

### 3.2 Reason values

| `reason` | Source | Meaning | What to do |
|---|---|---|---|
| `ps5_offline` | Breaker open, PS5 not configured, or the status probe failed or was invalid (`ps5_health.go`) | The gateway cannot get a valid status from `:8082`. Chat returns `503 PS5 is offline`, `Retry-After: 30`, and media returns the same. | Follow section 7 (crash and re-jailbreak). Also check Tailscale, the ACL, that the controller is on (token file present, `nexus-control.lan` present if needed), and that the bearer on the gateway matches the console (section 6). A wrong bearer also looks like `ps5_offline`, with no separate error. |
| `switching` | Controller state `switching` (engine or model switch pending or app restarting) | Normal during a switch, about 30 s for an engine switch. | Wait. If it lasts longer than 10 minutes, the gateway has given up. Check the app log. |
| `loading` | Controller state `loading` | A model is loading. `load_progress` on the controller shows progress. | Wait, about 100 s for a model load. If stuck, check logs. |
| `no_model` | Controller state `no_model` | No model is loaded or selected. | Load a model from the console, or call the controller's `POST /v1/nexus/models/<16-hex-id>/load` from `<gateway-host>`. NV: the gateway route for this is served by the orchestrator, not the PS5 directly. |
| `failed` | Controller state `failed` | The inference server failed to load or stopped. | Read the app and llama logs (section 3.3). Restart the title if the failure persists. |
| `restart_failed` | Controller `restart_failed` after a switch restart did not come back. Cleared by the next switch request. | The app did not return to the requested state. | Relaunch the title from the console, read the logs, then request the switch again. |
| `media_unavailable` | Engine is `media`, control state is ready or generating, and `:8083/health` is not `200` with `status: ok` | The media server is not ready. | Check `:8083/health` from `<gateway-host>` (`curl`, with no token). Wait, then restart the title. NV: the media server's own not-ready status code. |

Gateway errors that users see, for triage (`gateway/media_runner.go`, `gateway/main.go`):

| Status | Message | Meaning |
|---|---|---|
| 503 (Retry-After 30) | `PS5 is offline` | Breaker open or no status. |
| 429 (Retry-After 10) | `PS5 queue is full` | Per-subject or global queue limit reached. |
| 503 (Retry-After 30) | `PS5 could not switch to chat mode` / `PS5 could not switch to media mode` | Engine switch failed. Chat requests wait during a switch and do not show a 503 for it. |
| 503 (Retry-After 30) | `PS5 chat is not ready` | Chat switch did not reach ready within the 10-minute switch timeout. |
| 503 (Retry-After 30) | `PS5 media is not ready` | Media switch did not reach ready within the switch timeout. |
| 429 | `daily quota exceeded` | Quota for the permission is used up. |
| 403 | `no daily quota is configured for this AI permission` | No `NEXUS_AI_QUOTAS_JSON` rule for a media permission. |
| 502 | `AI media request failed` or the sanitized PS5 message | The PS5 failed while running the job. |

### 3.3 Logs

- Gateway: `journalctl -u nexus-gateway`. Authorization lines carry route, permission, outcome, reason and
  a hashed subject. Alert lines are `alert_event kind=<kind> count=<n>`. Usage failures are logged as
  `usage_event status=failed`.
- Console: the controller returns logs on `:8082`, `GET /v1/nexus/logs/app` and `GET /v1/nexus/logs/llama`
  (`?after=` for paging). Only the gateway host can reach them. Call them with the bearer, and do not
  print the token. Example, with the token held only in the file:

  ```sh
  curl -sS -H "Authorization: Bearer $(cat <token-file>)" http://<ps5-tailnet-ip>:8082/v1/nexus/logs/app
  ```

  The token briefly appears in the process list while `curl` runs. Use a 0600 `curl` config file if that
  matters on a shared host (NV: confirm this is acceptable for your host).

## 4. Alert responses

Alerts are always written to the gateway log. If `NEXUS_ALERT_WEBHOOK_URL` is set, they are also POSTed as
`{kind, count, timestamp, message}` (no request data). Each kind is deduplicated for 5 minutes. A webhook
that finds no free slot (4 in flight) is logged as `webhook=dropped`.

| Kind | Fires when | Response |
|---|---|---|
| `auth_failures` | Authentication failures in one UTC minute exceed `NEXUS_ALERT_AUTH_FAILURES_PER_MINUTE` (default 20). | Check the authorization log lines for `outcome` and `reason`. Look for expired tokens from a legitimate client first. If the failures are from an unknown source, check the Cloudflare Access policy and the Worker logs. Rotate the Access application only if you have evidence of compromise. There is no IP in the log, by design. |
| `quota_hit` | A media quota is exceeded. Fires on every hit, deduplicated. | Normal when a user reaches a limit. Find the subject by its hash in the usage log (section 9). Change `NEXUS_AI_QUOTAS_JSON` only on purpose. Quotas reset at UTC midnight. |
| `ps5_offline` | The PS5 goes from online to offline (transition only). The watcher assumes online before its first poll, so a PS5 already offline at gateway start alerts on the first poll. A healthy first poll alerts nothing. | Section 7. |
| `ps5_online` | The PS5 goes from offline to online. | Informational. Confirm `ready` and `engine` in section 3. |
| `request_spike` | Requests in one UTC minute exceed `NEXUS_ALERT_REQUESTS_PER_MINUTE` (default 600). | Check the usage log for one subject or one route. A client retry loop is the usual cause. Lower the subject's traffic or the rate limit (`NEXUS_AI_REQUESTS_PER_MINUTE`) if it is abuse. |
| `restart_failed` | The poll reports `reason: restart_failed`. | See section 3.2. The alert repeats at most once per 5 minutes while the state lasts. |

## 5. Token provisioning, format and rules

Facts that the rotation depends on:

- **Console file:** `nexus-control.token`, one line, `sha256:<64 lowercase hex>`, mode 0600 or stricter
  (`README.md:97-103`). It holds only the SHA-256 of the bearer. The raw token is never on the console.
- **Console bearer rule:** 32 to 128 characters from `A-Z a-z 0-9 _ -` (`native/nexus_control.cpp:98-120`).
  The gateway accepts 32 or more printable ASCII characters (`gateway/secrets.go`). A 64-character
  hex token meets both rules.
- **Trailing newline:** unpinned builds trim whitespace (`native/nexus_control.cpp:296-297`). Pinned
  builds require the exact 72-byte record with a trailing newline (`native/nexus_control.cpp:286-295`).
  Writing `sha256:<hex>` followed by one `\n` works for both.
- **Hashing:** the console hashes the bearer bytes exactly as sent. Hash the token without a newline.
- **Gateway file:** `secretLoadFile` strips one trailing newline and rejects symlinks, group or other
  permissions (on non-Windows), and anything over 4096 bytes.
- **Media port:** `:8083` checks the same token file (`native/nexus_media.hpp`).

### 5.1 Pinned or unpinned build (decides whether a rebuild is needed)

| Build | How to tell | Rotation |
|---|---|---|
| Unpinned | Built without `NEXUS_PS5_CONTROL_PIN_HEADER` (`patches/0006-nexus-provisioning-pin.patch`). | New digest file on the console, then relaunch the title. No rebuild. |
| Pinned | Built with `NEXUS_PS5_CONTROL_PIN_HEADER`, which compiles `NEXUS_CONTROL_REQUIRE_PROVISIONING_PIN` and a `NEXUS_CONTROL_PROVISIONING_DIGEST` define (`patches/0006`, `patches/0012`, `docs/TESTING.md:11-42`). | **Rebuild required** for every bearer change. The digest is compiled in and must equal the file. Rollback binaries keep their old pin. |

Whether the deployed build is pinned is not visible from the gateway. Check the build record for
`NEXUS_PS5_CONTROL_PIN_HEADER` (NV: no host-side record location is in the repositories).

Rebuild is also required if the controller code changes. A new bearer alone never needs a source change
in the unpinned case.

### 5.2 Rotation, step by step

Do this in a maintenance window with the queue empty. Keep the old values until the new ones are verified.

**Step 1. Preconditions.**

- `GET /v1/ai/status`: `online: true`, `queue_length: 0`.
- Back up the current gateway token (`<gateway-host>:/etc/nexus-gateway/ps5-token`) and the current
  console file (copy it through the same channel you will use to write it). Keep both private.

**Step 2. Generate the new bearer on `<gateway-host>`.**

```sh
umask 077
openssl rand -hex 32 > /etc/nexus-gateway/ps5-token.next
```

Do not put the file in `/etc/nexus-gateway/ps5-token` yet. The running gateway still needs the old one.

**Step 3. Derive the console digest (no newline in the hash input).**

```sh
DIGEST=$(tr -d '\n' < /etc/nexus-gateway/ps5-token.next | sha256sum | cut -d' ' -f1)
printf 'sha256:%s\n' "$DIGEST" > /etc/nexus-gateway/nexus-control.token.next
chmod 600 /etc/nexus-gateway/nexus-control.token.next
wc -c /etc/nexus-gateway/nexus-control.token.next   # expect 72
unset DIGEST
```

Check that the file has exactly one line and the prefix `sha256:` before transfer. Never transfer the raw
`ps5-token.next`.

**Step 4. Write the digest to the console.**

- Copy `nexus-control.token.next` to the console as `/data/PS5LM/nexus-control.token` (or
  `/download0/PS5LM/nexus-control.token` in the sandbox). Mode 0600 or stricter.
- Transfer method: NV. `docs/TESTING.md:97-101` records FTP :2121 as an accepted risk on the owner's
  trusted LAN. Do not rely on it for secrets unless the risk is accepted for that session.
- After transfer: confirm mode, size (72 or 71 bytes) and the single line. Do not print the file in a
  shared log.

**Step 5. Relaunch the title on the console.** The controller reads the file only at launch
(`native/nexus_control.cpp:211`). Until the title is back, the gateway reports `ps5_offline`.

**Step 6. Swap the gateway credential and restart.**

```sh
sudo install -m 600 -o root -g root /etc/nexus-gateway/ps5-token.next /etc/nexus-gateway/ps5-token
sudo systemctl restart nexus-gateway
```

Keep the old file as a private backup until verified. Restart is required: systemd copies the
`LoadCredential` file when the unit starts. The comment in `gateway/.env.example` says the bearer is
read per request and "rotation needs no restart". That is not what systemd does for a file on disk, and
the startup check at `gateway/main.go:332-338` loads it only once. Treat restart as required (NV: confirm
on the host).

**Step 7. Verify.**

- From `<gateway-host>`, with the new bearer, `GET /v1/nexus/status` on `:8082` returns `200`
  (`curl -sS -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $(cat /etc/nexus-gateway/ps5-token)" http://<ps5-tailnet-ip>:8082/v1/nexus/status`).
- The same call with the old bearer returns `401`.
- Without any bearer, `:8082` returns `401`.
- `journalctl -u nexus-gateway` shows no repeated probe failures. Within about 30 s, `GET /v1/ai/status`
  reports `online: true`.
- A test chat and a test media job from a test subject succeed.

**Step 8. Retire the old values.** After verification, delete the old gateway file and the old console
digest backup. Keep them only if a rollback plan needs them, and delete them when that window closes
(NV: retention period is an owner decision). Record the date of rotation in the ops log, never the value.

### 5.3 Pinned build variant

Replace step 4 with the following, and do it before the console relaunch.

1. Generate the header from the new bearer's digest, not from the console file. `docs/TESTING.md:19-27`
   says the digest comes from the SHA-256 of the bearer in the gateway's private configuration. Put the
   header in an owner-only (0600) private build directory outside git.
2. Build with `NEXUS_PS5_CONTROL_PIN_HEADER=<private-header-path>` using the pinned build tree
   (`./prepare-build.sh` and `scripts/build-app.sh`, NV: exact recipe for your build).
3. Deploy the new title binary. Keep the previous title build and its digest so rollback works
   (`docs/TESTING.md:36-41`).
4. Write `nexus-control.token` as in step 4 (exact 72 bytes with a trailing newline, mode 0600).
5. Relaunch, then do steps 6 to 8.

Rollback for a pinned build: restore the old title binary and the old console digest together. The old
binary only accepts its own pin.

## 6. Crash and re-jailbreak recovery

Symptoms: `GET /v1/ai/status` shows `online: false` and `reason: ps5_offline`. Chat and media return
`503 PS5 is offline` with `Retry-After: 30`. The `ps5_offline` alert fires once, on the transition.

What the gateway does meanwhile:

- Each probe fails and the breaker opens after 3 failures (about 15 s at a 5 s poll). It then sends one
  probe every 30 s.
- New chat and media requests are refused at the door with `503` and are not queued. Requests already
  in the queue keep waiting, because the queue has no timeout of its own. `NEXUS_AI_MAX_QUEUE_WAIT` is a
  fairness threshold, not a timeout (`gateway/ps5_scheduler.go`). Their end is set by the client's
  request timeout (NV: check the worst case on the host).

Recovery:

1. Confirm the network before touching the console. On `<gateway-host>`, check `tailscale status` and
   reach `<ps5-tailnet-ip>` (NV: tooling on your host). If the PS5 is reachable but `:8082` is not, check
   `nexus-control.token` and `nexus-control.lan` (section 2.2).
2. Reboot the console if it is hung. Crash recovery may need a reboot and a fresh jailbreak
   (`PSllama` README, cited in the plan as `[PSL] README.md:362`; NV: not in this repository). Use only the
   method you own and trust. This runbook does not describe jailbreak or exploit steps.
3. After the reboot, bring the payload chain back up with the method you use (NV). Payload and loader
   ports are part of the accepted-risk set in `docs/TESTING.md:97-101`. Keep them off when not needed.
4. Launch the NexusAI title with the same title ID as before, unless you deliberately test a new one
   (`docs/TESTING.md:53-68`).
5. Check the state: `:8082` `GET /v1/nexus/status` returns `state`, `engine`, `model` and `ready`. If the
   engine was media, `model.txt` line 1 is `media` and the app returns to media mode on its own
   (`patches/0011`).
6. Wait for `GET /v1/ai/status` to report `online: true`. Expect up to about 30 s after the title is
   back (the breaker cooldown).
7. Run one chat and one media test from a test subject.
8. If the crash repeats, read the app and llama logs (section 3.3) and keep the original title as the
   rollback (section 8).

Do not restart the gateway to "clear" an offline state. It does not affect the PS5, and it resets the
breaker and the queue.

## 7. Queue and quota operations

### 7.1 Queue settings

| Variable | Default | Range | Effect |
|---|---|---|---|
| `NEXUS_AI_QUEUE_PER_SUBJECT` | 3 | 1 to 20 | Waiting jobs allowed for one subject. Beyond this, `429 PS5 queue is full` (`Retry-After: 10`). |
| `NEXUS_AI_QUEUE_GLOBAL` | 20 | at least `QUEUE_PER_SUBJECT`, at most 200 | Waiting jobs allowed in total. |
| `NEXUS_AI_MAX_QUEUE_WAIT` | `120s` | 10 s to 30 min | A job that has waited longer wins the next slot (fairness). It is not a timeout. |
| `NEXUS_PS5_POLL_INTERVAL` | `5s` | 1 s to 5 min | Health poll period. |
| `NEXUS_AI_REQUEST_TIMEOUT` | `15m` | 10 s to 30 min | Upper bound for a chat run. |
| `NEXUS_AI_MAX_CONCURRENT` | 2 | 1 to 16 | Used by the direct (non-PS5) inference path. The PS5 path uses the queue instead (NV: confirm no other use). |

Hard-coded values (change only in code): breaker threshold 3, cooldown 30 s, probe timeout 3 s, switch
timeout 10 min, media result TTL 10 min and store cap 128 MiB (`gateway/media_runner.go`).

Each subject is served least recently first inside the same tier, so one subject's burst cannot run ahead
of others on the same engine (`gateway/ai_status.go`).

### 7.2 Quotas

Media permissions need a daily quota rule. A permission with no rule is refused with `403`. Chat
(`ai.chat`) has no quota.

Permission names: `ai.image`, `ai.video`, `ai.audio`, `ai.voice`, `ai.voice.clone` (`gateway/quota.go`).
The subject must also be granted the permission in `NEXUS_SUBJECT_PERMISSIONS_JSON`.

Example (placeholder numbers, tune them yourself):

```sh
NEXUS_AI_QUOTAS_JSON='{"ai.image":{"unit":"requests","limit":10},"ai.video":{"unit":"requests","limit":2},"ai.audio":{"unit":"seconds","limit":600},"ai.voice":{"unit":"seconds","limit":600}}'
```

- `unit` is `requests` or `seconds`. Audio and voice are charged in seconds.
- An empty value means no quotas, so every media permission is refused.
- Counts reset at UTC midnight. The meter is in memory, so a gateway restart resets all counts to zero
  (NV: confirm this is acceptable).

### 7.3 Refunds

A quota unit is charged at admission, after the PS5-online check and before queueing. It is refunded
when the PS5 never ran the generation:

| Case | Refunded |
|---|---|
| Queue full at admission (`429 PS5 queue is full`) | Yes |
| Engine switch refused or failed (`PS5 could not switch to media mode`) | Yes |
| Job cancelled before the run started | Yes |
| Job cancelled or failed after the run started | No |
| PS5 returned an error during the run (`502`) | No |

Refunds do nothing once the UTC day has rolled over (`gateway/quota.go`). A refund never takes usage below
zero.

## 8. Rollback

- Gateway: keep the previous binary and the previous `nexus.env`. Restore them and restart the unit. The
  model-state journal is kept across a restart.
- Console title: follow `docs/TESTING.md:122-128`. Stop the experimental title, keep the logs (`app.log`,
  `llama.log`), remove or restore the controller's token and `nexus-control.lan` marker in the test state,
  restore the original upstream settings from the verified backup if they changed, then launch the original
  title. Never delete or overwrite the original `PPSA99581` binary.
- Pinned builds: restore the old title binary and its matching digest together (section 5.3).

## 9. Adding a model

1. **Licence first.** Check the model in NexusAI `docs/MODELS-LICENSES.md`. That file does not exist yet
   (plan U-65, not done). Each row needs a size and a licence, or "NV". Stop if the licence is
   restrictive for your use (the plan lists Stability Community and Hunyuan Community as restrictive).
2. **Folder.** Image models live in `/data/PS5LM/media/<model-id>/` (`native/nexus_media_image.cpp:31`).
   The folder name is the model ID. Keep IDs free of `..`, `/` and `\` (`gateway/media_validate.go`).
   - An image model needs `sd-model.conf`. Only the keys in `kKnownKeys` are accepted
     (`native/nexus_sd_service.cpp:37-41`), and component paths are relative to the model folder. A path
     may climb to a sibling folder with `..` but not above `/data/PS5LM/media`.
   - Video folder layout: NV (not verified in this pass). The gateway default is `wan2.1-t2v-1.3b`.
   - STT and TTS models are fixed in the gateway: `whisper/ggml-large-v3-turbo-q5_0.bin`
     (`gateway/media_stt.go:14`) and `qwen3-tts-1.7b` (`gateway/media_tts.go:12`). Changing them is a code
     change, not a folder change.
3. **Gateway allow-list.** Add the ID to the constant map:
   - images: `mediaImageModels` in `gateway/media_image.go:16` (currently `sd-turbo-fp16`,
     `flux2-klein-4b`);
   - video: `mediaVideoModels` in `gateway/media_video.go:16` (currently `wan2.1-t2v-1.3b`).
   The allow-lists are not environment settings. Run `CGO_ENABLED=0 go test ./...`, rebuild, and restart
   the gateway.
4. **NexusOnline.** Add the ID and label to `IMAGE_MODELS` in
   `NexusOnline/src/app/image-core.ts` (lines 8 and 9 today). The comment there says it must match the
   gateway allow-list. The video list in NexusOnline: NV.
5. **Test.** Run one generation from a test subject with a quota rule. Check the usage line (section 10).

## 10. Usage log

- Location: `NEXUS_USAGE_DIR` (system unit default `/var/lib/nexus-gateway/usage`). The directory is created
  0700 and files are created 0600 (`gateway/usage.go`).
- File name: `usage-YYYY-MM.jsonl`, by UTC month. One JSON object per line.
- Fields: `ts`, `subject_hash` (first 8 bytes of SHA-256 of the subject, as 16 hex characters),
  `route`, `engine` (`llm` or `media`), `model`, `outcome`, `queue_ms`, `run_ms`, `in_bytes`, `out_bytes`.
- It never contains prompts, messages, audio, images or output text. The record type has no field for
  them (`gateway/usage.go`).
- The log is written only when the PS5 is configured (`startPS5Services`). Chat and media both write lines.
- Use it to find a subject by hash (compute `sha256(subject)` for a known subject, and take the first 16
  hex characters) or to count requests per route. Do not join it with any other log that stores prompts.
- Retention and rotation: NV (no logrotate policy is in the repository).

## 11. Checklist for a new operator

- [ ] Tailscale ACL matches `docs/tailscale-acl.example.hujson` (one rule, gateway tag to PS5 tag, 8081 to 8083), and the live policy matches the file.
- [ ] `nexus-control.token` is on the console (mode 0600, one `sha256:` line) and the title is relaunched after any change.
- [ ] `nexus-control.lan` is present only when the ACL and firewall are ready.
- [ ] `/etc/nexus-gateway/ps5-token` is 0600 root-owned, and the gateway was restarted after the last change.
- [ ] `GET /v1/ai/status` reports `online: true`, and a test chat and test media job succeed.
- [ ] Alert webhook is HTTPS and clean (no query, no credentials), or left empty.
- [ ] `NEXUS_AI_QUOTAS_JSON` has a rule for every media permission you grant.

## 12. Open items (Needs Verification)

- Whether the deployed console build is pinned (section 5.1).
- Exact transfer method for the console digest file (section 5.2 step 4).
- Whether a gateway restart is needed for a bearer change in practice (section 5.2 step 6). The
  `.env.example` comment says no.
- Launch and quit method for the console title (section 2.2).
- Gateway route or orchestrator path for `POST /v1/nexus/models/{id}/load` (section 3.2).
- Media server not-ready status code (section 3.2).
- Worst-case wait for queued requests while the PS5 is offline (section 6).
- Video model folder layout and the NexusOnline video list (section 9).
- Retention policy for usage logs and old tokens, and whether quota counts reset on restart (sections 5, 7, 10).
