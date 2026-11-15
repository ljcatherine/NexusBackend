"""Pinned, isolated DeepSeek Harness (DSH) agent loop; Nexus retains every privileged operation.

DSH runs in the same networkless, read-only, unprivileged container as the
Hermes engine and speaks the same worker protocol (integrations/dsh/worker.mjs):
its model is a loopback shim whose requests become Nexus inference frames, and
its only tools are Nexus's scoped workspace tools over MCP. All of DSH's own
tools (files, shell, web, subagents) are disabled inside the worker.

DSH runs are read-only: no patch proposals, so no proposal checkpoint or
resume. Everything else (tokenizer preflight, 90% context bound, six scoped
actions, PS5 transport, cancellation, container quarantine) is the Hermes
engine's code.
"""
from __future__ import annotations

from hermes_engine import DockerWorker, HermesEngine, HermesUnavailable

DSH_VERSION = '0.2.2-rc.1'
PROTOCOL = '1'


def dsh_worker(executable, image):
    """Docker worker for the pinned DSH image built by scripts/build-dsh-worker.sh."""
    return DockerWorker(executable, image, engine='DeepSeek Harness',
        labels={'org.nexus.dsh.version': DSH_VERSION, 'org.nexus.dsh.protocol': PROTOCOL},
        name_prefix='nexus-dsh-', entrypoint=('/usr/local/bin/node', '/opt/nexus/worker.mjs'),
        # Node runs three processes (worker, DSH, MCP bridge) with their thread pools.
        pids=256, tmpfs_mb=256)


class DshEngine(HermesEngine):
    def run(self, controller, messages, model, context_window, output_budget, workspace,
            report, cancelled, run_state, proposal_checkpoint, resume=None, *, compacted=False):
        if resume is not None:
            raise HermesUnavailable('DeepSeek Harness runs cannot resume a proposal')
        # No proposal checkpoint: the tool set stays search/read_file/Git inspection.
        return super().run(controller, messages, model, context_window, output_budget, workspace,
                           report, cancelled, run_state, None, None, compacted=compacted)
