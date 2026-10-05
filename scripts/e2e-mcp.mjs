// Drives a Devdooth-leased browser through upstream Playwright MCP.
//
//   DEVDOOTH_URL=http://127.0.0.1:18080 DEVDOOTH_TOKEN=... node scripts/e2e-mcp.mjs
//
// This is the V1 acceptance path: an MCP client talks to `devdooth mcp`, which
// leased a browser from a worker and handed it to Playwright MCP. Devdooth
// implements no browser tools of its own.
import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { StdioClientTransport } from '@modelcontextprotocol/sdk/client/stdio.js';

const url = process.env.DEVDOOTH_URL;
const token = process.env.DEVDOOTH_TOKEN;
if (!url || !token) {
  console.error('set DEVDOOTH_URL and DEVDOOTH_TOKEN');
  process.exit(2);
}

const mcpArgs = ['mcp', '--url', url, '--token', token];
if (process.env.DEVDOOTH_NODE) mcpArgs.push('--node', process.env.DEVDOOTH_NODE);
if (process.env.DEVDOOTH_PROFILE) mcpArgs.push('--profile', process.env.DEVDOOTH_PROFILE);

const transport = new StdioClientTransport({
  command: './bin/devdooth',
  args: mcpArgs,
  env: process.env,
});

const client = new Client({ name: 'devdooth-e2e', version: '0.0.0' });
let failed = false;
try {
  await client.connect(transport);
  const { tools } = await client.listTools();
  console.log(`tools available: ${tools.length}`);
  if (tools.length === 0) throw new Error('MCP server exposed no tools');

  const expected = 'mcp-devdooth';
  await client.callTool({
    name: 'browser_navigate',
    arguments: { url: `data:text/html,<title>${expected}</title><h1 id="m">hi from mcp</h1>` },
  });

  const result = await client.callTool({
    name: 'browser_evaluate',
    arguments: { function: '() => document.title' },
  });
  const text = (result.content || []).map((c) => c.text || '').join('\n');
  console.log(`browser_evaluate: ${text.trim()}`);
  if (!text.includes(expected)) throw new Error(`expected ${expected} in tool output`);
  console.log('OK: LLM-style MCP client drove a Devdooth-leased browser');
} catch (err) {
  console.error('FAIL:', err.message);
  failed = true;
} finally {
  await client.close().catch(() => {});
}
process.exit(failed ? 1 : 0);
