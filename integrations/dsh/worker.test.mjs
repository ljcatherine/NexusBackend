// node --test integrations/dsh/worker.test.mjs
import assert from 'node:assert/strict';
import test from 'node:test';
import { DISABLED_PLUGINS, overlay, sse, task, toNexus } from './worker.mjs';
import { handle, listing } from './mcp.mjs';

test('DSH messages become Nexus history with one leading system message', () => {
  const out = toNexus([
    { role: 'system', content: 'DSH prompt' },
    { role: 'user', content: [{ type: 'text', text: 'Read a' }, { type: 'text', text: '.txt' }] },
    { role: 'assistant', content: null, tool_calls: [{ id: 'c1', type: 'function', function: { name: 'mcp__nexus__read_file', arguments: '{"path":"a.txt"}' } }] },
    { role: 'tool', tool_call_id: 'c1', content: 'A', name: 'mcp__nexus__read_file' },
  ]);
  assert.deepEqual(out.map(m => m.role), ['user', 'user', 'assistant', 'tool']);
  assert.equal(out[0].content, 'DeepSeek Harness instructions:\nDSH prompt');
  assert.equal(out[1].content, 'Read a.txt');
  assert.equal(out[2].tool_calls[0].function.name, 'read_file');
  assert.equal(out[3].name, 'read_file');
  assert.throws(() => toNexus([{ role: 'assistant', tool_calls: [{ function: {} }, { function: {} }] }]), /sequential/);
});

test('Nexus completions stream back with DSH tool names', () => {
  const body = sse({ id: 'n1', created: 1, choices: [{ message: { content: null, tool_calls: [{ id: 'c1', function: { name: 'search', arguments: '{"query":"x"}' } }] }, finish_reason: 'tool_calls' }] }, 'm');
  const chunks = body.trim().split('\n\n').map(l => l.replace(/^data: /, ''));
  assert.equal(chunks.at(-1), '[DONE]');
  const first = JSON.parse(chunks[0]);
  assert.equal(first.choices[0].delta.tool_calls[0].function.name, 'mcp__nexus__search');
  assert.equal(JSON.parse(chunks[1]).choices[0].finish_reason, 'tool_calls');
  assert.ok(JSON.parse(chunks[2]).usage);
});

test('overlay has the shim as the only model, the MCP bridge and no DSH tools', () => {
  const y = overlay({ model: 'm"x', context_window: 65536, output_budget: 4096 }, 4321, []);
  assert.match(y, /baseURL: "http:\/\/127\.0\.0\.1:4321\/v1"/);
  assert.match(y, /model: "m\\"x"/);
  assert.match(y, /- insert:\n    - id: mcp-nexus/);
  for (const id of ['tool-bash', 'tool-fs', 'tool-web', 'session-title-llm', 'mcp-resources']) {
    assert.ok(DISABLED_PLUGINS.includes(id));
    assert.match(y, new RegExp(`- id: ${id}\\n  disabled: true`));
  }
});

test('task carries Nexus system text, earlier turns and the request', () => {
  const t = task({ system: 'S', history: [{ role: 'user', content: 'q1' }, { role: 'assistant', content: 'a1' }, { role: 'user', content: 'q2' }] });
  assert.equal(t, 'S\n\nConversation so far:\nuser: q1\n\nassistant: a1\n\nCurrent request:\nq2');
  assert.throws(() => task({ system: 'S', history: [{ role: 'assistant', content: 'x' }] }));
});

test('MCP bridge lists Nexus tools and forwards only those', async () => {
  const tools = [{ type: 'function', function: { name: 'read_file', description: 'd', parameters: { type: 'object' } } }];
  assert.deepEqual(listing(tools), [{ name: 'read_file', description: 'd', inputSchema: { type: 'object' } }]);
  const sent = [];
  const send = { reply: (id, r) => sent.push({ id, r }), error: (id, code) => sent.push({ id, code }) };
  await handle({ id: 1, method: 'initialize', params: { protocolVersion: 'v' } }, send);
  assert.equal(sent[0].r.protocolVersion, 'v');
  await handle({ method: 'notifications/initialized' }, send);
  assert.equal(sent.length, 1);
  await handle({ id: 2, method: 'tools/call', params: { name: 'shell', arguments: {} } }, send);
  assert.equal(sent[1].code, -32602);
});
