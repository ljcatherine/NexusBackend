// Credential-free DeepSeek Harness worker. Launched only in Nexus's networkless container.
//
// DSH owns its agent loop. Nexus owns all model I/O and workspace operations:
// - DSH's model is a loopback OpenAI-compatible shim; each request becomes an
//   `inference` frame answered by Nexus (PS5 inference, budgets, quarantine).
// - DSH's only tools are Nexus's scoped tools, served by mcp.mjs over stdio;
//   each call becomes a `tool` frame. Every built-in DSH tool is disabled.
// Frames are the Hermes worker protocol (one JSON object per line on stdio).
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import net from 'node:net';
import { StringDecoder } from 'node:string_decoder';

const LIMIT = 1024 * 1024;
const PREFIX = 'mcp__nexus__';
// The container's private tmpfs and pinned DSH install; overridable only for local tests.
const TMP = process.env.NEXUS_WORKER_TMP || '/tmp';
const SOCKET = `${TMP}/nexus-mcp.sock`;
const DSH = process.env.NEXUS_DSH_BIN || '/opt/dsh/node_modules/.bin/dsh';
const MCP = new URL('./mcp.mjs', import.meta.url).pathname;
// Every DSH plugin that offers the model a tool of its own.
export const DISABLED_PLUGINS = [
  'tool-bash', 'tool-pwsh', 'tool-jobs', 'tool-fs', 'tool-fs-search', 'tool-web', 'tool-skill',
  'tool-subagent', 'tool-subagent-control', 'tool-subagent-fork', 'tool-workflow', 'tool-todo',
  'tool-goal', 'tool-plugin-manager', 'tool-working-directory', 'web-search-deepseek',
  'web-fetch-http', 'mcp-resources',
  // A second model call per run (session titles); Nexus allows one model call at a time.
  'session-title-llm',
];

// ---- framed protocol with Nexus (stdin/stdout) ----
const waiting = [];
const lines = [];
let pending = '';
const decoder = new StringDecoder('utf8');
function listen() {
  process.stdin.on('data', chunk => {
    pending += decoder.write(chunk);
    if (pending.length > LIMIT + 2) fail();
    let i;
    while ((i = pending.indexOf('\n')) >= 0) {
      const line = pending.slice(0, i);
      pending = pending.slice(i + 1);
      const w = waiting.shift();
      w ? w(line) : lines.push(line);
    }
  });
  process.stdin.on('end', () => fail());
}
const receive = () => new Promise(resolve => {
  const line = lines.shift();
  line === undefined ? waiting.push(resolve) : resolve(line);
}).then(line => JSON.parse(line));

function send(value) {
  const encoded = JSON.stringify(value);
  if (Buffer.byteLength(encoded) > LIMIT) throw new Error('worker frame too large');
  process.stdout.write(encoded + '\n');
}

let chain = Promise.resolve();
/** One Nexus operation at a time, in order. */
function rpc(kind, payload) {
  const run = chain.then(async () => {
    send({ kind, ...payload });
    const reply = await receive();
    if (reply.ok !== true) throw new Error('Nexus refused the operation');
    return reply.result;
  });
  chain = run.catch(() => {});
  return run;
}

let done = false;
// Local debugging only; Nexus discards the container's stderr.
const DEBUG = process.env.NEXUS_WORKER_DEBUG === '1';
function fail(err) {
  if (DEBUG && err) process.stderr.write(`worker: ${err.stack ?? err}\n`);
  if (done) return;
  done = true;
  // No prompts, model output, source text or DSH error text in the frame.
  try { send({ kind: 'failed' }); } catch {}
  process.exit(1);
}

// ---- message translation between DSH and Nexus's tool names ----
export function text(content) {
  if (content == null) return null;
  if (typeof content === 'string') return content;
  if (Array.isArray(content)) return content.map(p => (p && p.type === 'text' ? p.text : '')).join('');
  return String(content);
}

/** DSH chat messages → Nexus history (Nexus tool names, plain-text content).
 * Nexus puts its own action protocol first as the only system message (chat
 * templates such as Qwen's reject a second one), so DSH's system/developer
 * instructions travel as a user message. */
export function toNexus(messages) {
  if (!Array.isArray(messages)) throw new Error('invalid messages');
  return messages.map(m => {
    const instructions = m.role === 'system' || m.role === 'developer';
    const role = instructions ? 'user' : m.role;
    const body = text(m.content);
    const out = { role, content: instructions ? `DeepSeek Harness instructions:\n${body ?? ''}` : body };
    if (Array.isArray(m.tool_calls) && m.tool_calls.length) {
      if (m.tool_calls.length !== 1) throw new Error('Nexus requires sequential tool calls');
      const c = m.tool_calls[0];
      out.tool_calls = [{ id: c.id, type: 'function', function: {
        name: String(c.function?.name ?? '').replace(PREFIX, ''), arguments: c.function?.arguments ?? '{}' } }];
    }
    if (role === 'tool') {
      out.tool_call_id = m.tool_call_id;
      if (m.name) out.name = String(m.name).replace(PREFIX, '');
    }
    return out;
  });
}

/** One Nexus chat.completion → the SSE chunks DSH's streaming client reads. */
export function sse(completion, model) {
  const choice = completion?.choices?.[0];
  const message = choice?.message ?? {};
  const delta = { role: 'assistant' };
  if (typeof message.content === 'string') delta.content = message.content;
  if (Array.isArray(message.tool_calls)) {
    delta.tool_calls = message.tool_calls.map((c, index) => ({ index, id: c.id, type: 'function',
      function: { name: PREFIX + c.function.name, arguments: c.function.arguments } }));
  }
  const base = { id: completion?.id ?? 'nexus', object: 'chat.completion.chunk', created: completion?.created ?? 0, model };
  return [
    { ...base, choices: [{ index: 0, delta, finish_reason: null }] },
    { ...base, choices: [{ index: 0, delta: {}, finish_reason: choice?.finish_reason ?? 'stop' }] },
    { ...base, choices: [], usage: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 } },
  ].map(c => `data: ${JSON.stringify(c)}\n\n`).join('') + 'data: [DONE]\n\n';
}

/** The task DSH receives: Nexus's system message, the earlier turns, then the request. */
export function task(request) {
  const history = [...request.history];
  const last = history.pop();
  if (!last || last.role !== 'user' || typeof last.content !== 'string') throw new Error('invalid request');
  const parts = [request.system];
  const earlier = history.filter(m => typeof m.content === 'string' && m.content);
  if (earlier.length) parts.push('Conversation so far:\n' + earlier.map(m => `${m.role}: ${m.content}`).join('\n\n'));
  parts.push('Current request:\n' + last.content);
  return parts.join('\n\n');
}

const q = s => JSON.stringify(String(s));
/** DSH overlay: the shim as the only model, Nexus's MCP tools, built-in tools off. */
export function overlay(request, port, tools) {
  const lines = [
    '- id: llm-pi-ai', '  config:', '    providers:', '      nexus:', '        displayName: "Nexus"',
    '        apiKeyEnv: NEXUS_SHIM_KEY', '        api: openai-completions',
    `        baseURL: ${q(`http://127.0.0.1:${port}/v1`)}`, '        models:',
    `          - id: ${q(request.model)}`, `            contextWindow: ${Number(request.context_window)}`,
    `            maxTokens: ${Number(request.output_budget)}`, '            reasoningEfforts: false',
    '- id: agent-default-model', '  config:', '    provider: nexus', `    model: ${q(request.model)}`,
    '- insert:', '    - id: mcp-nexus', "      name: '@deepseek-ai/dsh-mcp-client'", '      config:',
    '        serverName: nexus', '        transport: stdio', `        command: ${q(process.execPath)}`,
    `        args: [${q(MCP)}]`, '        env:', `          NEXUS_MCP_SOCKET: ${q(SOCKET)}`,
    `          NEXUS_MCP_TOOLS: ${q(JSON.stringify(tools))}`, '        failOnStartupError: true',
    '        reconnect:', '          enabled: false',
  ];
  for (const id of DISABLED_PLUGINS) lines.push(`- id: ${id}`, '  disabled: true');
  return lines.join('\n') + '\n';
}

async function main() {
  listen();
  const request = await receive();
  if (request.resume) throw new Error('DSH runs cannot resume a proposal checkpoint');
  const tools = request.tools ?? [];
  let history = [];

  // Model shim on loopback (the container has no other network).
  const shim = http.createServer((req, res) => {
    let body = '';
    req.setEncoding('utf8');
    req.on('data', c => { body += c; if (body.length > 8 * LIMIT) req.destroy(); });
    req.on('end', async () => {
      try {
        if (req.method === 'GET' && req.url === '/v1/models') {
          res.writeHead(200, { 'content-type': 'application/json' });
          return res.end(JSON.stringify({ object: 'list', data: [{ id: request.model, object: 'model' }] }));
        }
        if (req.method !== 'POST' || req.url !== '/v1/chat/completions') { res.writeHead(404); return res.end(); }
        const call = JSON.parse(body);
        if (DEBUG) process.stderr.write(`worker: tools offered: ${(call.tools ?? []).map(t => t.function?.name).join(', ')}\n`);
        const messages = toNexus(call.messages);
        const completion = await rpc('inference', { messages });
        const reply = completion?.choices?.[0]?.message;
        // The tool frame carries the conversation that requested the tool.
        if (reply?.tool_calls) history = [...messages, { role: 'assistant', content: null, tool_calls: reply.tool_calls }];
        res.writeHead(200, { 'content-type': 'text/event-stream' });
        res.end(sse(completion, request.model));
      } catch (e) {
        // A refused or failed inference ends the run; DSH must not retry it.
        fail(e);
      }
    });
  });
  await new Promise(r => shim.listen(0, '127.0.0.1', r));

  // Tool bridge for mcp.mjs (Unix socket in the private tmpfs).
  fs.rmSync(SOCKET, { force: true });
  const bridge = net.createServer(conn => {
    let buf = '';
    conn.setEncoding('utf8');
    conn.on('data', async c => {
      buf += c;
      const i = buf.indexOf('\n');
      if (i < 0) return;
      try {
        const { name, arguments: args } = JSON.parse(buf.slice(0, i));
        const result = await rpc('tool', { name: String(name), arguments: args ?? {}, history });
        conn.end(JSON.stringify({ ok: true, result: typeof result === 'string' ? result : JSON.stringify(result) }) + '\n');
      } catch (e) {
        conn.end(JSON.stringify({ ok: false }) + '\n');
        fail(e);
      }
    });
  });
  await new Promise(r => bridge.listen(SOCKET, r));
  fs.chmodSync(SOCKET, 0o600);

  fs.mkdirSync(`${TMP}/work`, { recursive: true, mode: 0o700 });
  fs.writeFileSync(`${TMP}/nexus-overlay.yml`, overlay(request, shim.address().port, tools), { mode: 0o600 });
  const child = spawn(DSH, ['--profile', 'headless', '--patch', `${TMP}/nexus-overlay.yml`, '--json',
    '--permission-mode', 'read-only', '--approval-policy', 'never', '-'], {
    cwd: `${TMP}/work`, stdio: ['pipe', 'pipe', DEBUG ? 'inherit' : 'ignore'],
    // Native addons load in place from the read-only image: /tmp is noexec, and
    // nothing executable may come from the writable tmpfs.
    env: { PATH: process.env.PATH, HOME: TMP, DSH_HOME: `${TMP}/dsh`, NARB_DISABLE_NATIVE_CACHE: '1',
      NEXUS_SHIM_KEY: 'nexus-worker-no-network', NODE_ENV: 'production' },
  });
  child.stdin.end(task(request));
  let out = '';
  let final = null;
  child.stdout.setEncoding('utf8');
  child.stdout.on('data', c => {
    out += c;
    let i;
    while ((i = out.indexOf('\n')) >= 0) {
      const line = out.slice(0, i);
      out = out.slice(i + 1);
      try { const e = JSON.parse(line); if (e.type === 'final') final = e; } catch {}
    }
  });
  const code = await new Promise(r => child.on('close', r));
  const answer = final?.reason?.kind === 'completed' ? final.text : null;
  if (code !== 0 || typeof answer !== 'string' || !answer.trim()) throw new Error('DSH did not finish successfully');
  done = true;
  send({ kind: 'done', response: answer });
  process.exit(0);
}

if (process.argv[1] && import.meta.url === new URL(process.argv[1], 'file://').href) {
  main().catch(e => fail(e));
}
