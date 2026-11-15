<div align="center">

<img src="../../docs/design/nexus-icon.svg" width="64" alt="Nexus">

# DeepSeek Harness engine for NexusAI

**Optional DeepSeek Harness (DSH) agent loop, running networkless behind Nexus's own inference and tools.**

![engine: optional](https://img.shields.io/badge/engine-optional-0B1430?style=flat-square&labelColor=070C18) ![runs in: Docker, no network](https://img.shields.io/badge/runs%20in-Docker,%20no%20network-0B1430?style=flat-square&labelColor=070C18&logo=docker&logoColor=9CBCFF) ![deployed: not yet](https://img.shields.io/badge/deployed-not%20yet-0B1430?style=flat-square&labelColor=070C18)

<sub>Part of <a href="../../README.md">Nexus</a> · private</sub>

</div>

---

An owned agent run with `engine:"dsh"` runs the official
[DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) (`@deepseek-ai/dsh`, pinned in
`version` and `package-lock.json`) as its agent loop. It uses the Hermes engine's worker protocol and
container, so Nexus keeps every privileged operation:

| DSH wants | What it gets |
|---|---|
| A model | A loopback OpenAI-compatible shim in the container (`worker.mjs`). Each request becomes an `inference` frame; Nexus tokenizes it, applies the 90% context bound and calls the PS5 over its existing transport. |
| Tools | Only Nexus's scoped tools (`search`, `read_file`, opt-in Git inspection), served by `mcp.mjs` over MCP and seen as `mcp__nexus__*`. Each call becomes a `tool` frame that Nexus authorizes and performs. |
| Its own tools | All disabled in the per-run overlay: files, shell, jobs, web, skills, subagents, workflows, todos, goals, plugin manager, MCP resources, and the session-title model call. |

Runs are **read-only**: no patch proposals, so no proposal checkpoint or resume
(`allow_proposals` with `engine:"dsh"` is refused). Use the Nexus or Hermes engine for proposals.

## Container

Same as Hermes: `--network none`, read-only root, `--cap-drop ALL`, `no-new-privileges`, user
65534, 1 GiB memory, 1 CPU, noexec tmpfs `/tmp` (256 MiB, DSH's home and session files). The pids
limit is 256 because three Node processes run (worker, DSH, MCP bridge). Native addons load in
place from the read-only image (`NARB_DISABLE_NATIVE_CACHE=1`); nothing executable comes from
`/tmp`. The image must carry `org.nexus.dsh.version` equal to the orchestrator's pinned version and
`org.nexus.dsh.protocol=1`.

DSH's own system prompt is sent as a user message: Nexus's action protocol stays the only system
message, because chat templates such as Qwen's reject a second one.

## Build and enable

```bash
scripts/build-dsh-worker.sh                 # prints the image ID (sha256:…)
NEXUS_DSH_IMAGE=sha256:…                    # orchestrator environment
NEXUS_DSH_DOCKER=/usr/bin/docker            # default
```

Without `NEXUS_DSH_IMAGE`, `engine:"dsh"` answers 503 and nothing starts.

## Verified (15 November 2026, owner's NixOS PC)

- `python3 -m unittest discover -p 'test_*.py'` in `agent-orchestrator`: 199 passed, with the real
  image (`NEXUS_DSH_TEST_IMAGE`, `NEXUS_DSH_TEST_DOCKER`): DSH's loop in the container with fixture
  PS5 replies (`read_file`, then an answer), and an isolation probe (no egress, no mounts, uid 65534,
  read-only root).
- `node --test integrations/dsh/worker.test.mjs`: 5 passed.
- The real `Controller` with `engine:"dsh"` against NexusAI on a PS5 (Ternary Bonsai 2 27B, 65,536
  context), no workspace: DSH answered through Nexus's PS5 transport in 7 s.

**Not verified:** deployment on the Debian orchestrator host (not done), a run with a real workspace
through the workspace agents, Git inspection tools, and runs from Nexus Online or the iPhone app.
