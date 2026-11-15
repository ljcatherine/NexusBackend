// Minimal stdio MCP server that DSH launches inside the worker container. It
// lists Nexus's scoped tools and forwards each call to worker.mjs over a
// private Unix socket; Nexus decides and performs the operation.
import net from 'node:net';
import readline from 'node:readline';

const TOOLS = JSON.parse(process.env.NEXUS_MCP_TOOLS || '[]');
const SOCKET = process.env.NEXUS_MCP_SOCKET;

export function listing(tools) {
  return tools.map(t => ({ name: t.function.name, description: t.function.description,
    inputSchema: t.function.parameters }));
}

function call(name, args) {
  return new Promise((resolve, reject) => {
    const conn = net.createConnection(SOCKET);
    let buf = '';
    conn.setEncoding('utf8');
    conn.on('connect', () => conn.write(JSON.stringify({ name, arguments: args }) + '\n'));
    conn.on('data', c => { buf += c; });
    conn.on('end', () => {
      try {
        const reply = JSON.parse(buf);
        reply.ok ? resolve(reply.result) : reject(new Error('refused'));
      } catch (e) { reject(e); }
    });
    conn.on('error', reject);
  });
}

const reply = (id, result) => process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id, result }) + '\n');
const error = (id, code, message) => process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id, error: { code, message } }) + '\n');

export async function handle(msg, send = { reply, error }, invoke = call) {
  const { id, method, params } = msg;
  if (id === undefined) return; // notifications
  switch (method) {
    case 'initialize':
      return send.reply(id, { protocolVersion: params?.protocolVersion ?? '2025-06-18',
        capabilities: { tools: { listChanged: false } }, serverInfo: { name: 'nexus', version: '1' } });
    case 'ping':
      return send.reply(id, {});
    case 'tools/list':
      return send.reply(id, { tools: listing(TOOLS) });
    case 'tools/call': {
      const name = params?.name;
      if (!TOOLS.some(t => t.function.name === name)) return send.error(id, -32602, 'unknown tool');
      try {
        const text = await invoke(name, params?.arguments ?? {});
        return send.reply(id, { content: [{ type: 'text', text: String(text) }], isError: false });
      } catch {
        return send.reply(id, { content: [{ type: 'text', text: 'Nexus refused or failed this operation.' }], isError: true });
      }
    }
    default:
      return send.error(id, -32601, 'method not found');
  }
}

if (process.argv[1] && import.meta.url === new URL(process.argv[1], 'file://').href) {
  readline.createInterface({ input: process.stdin }).on('line', line => {
    let msg;
    try { msg = JSON.parse(line); } catch { return; }
    handle(msg);
  });
}
