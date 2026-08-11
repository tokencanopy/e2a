// mock-mcp.mjs — a stub e2a MCP server for lane fixtures.
// Serves the fixture inbox (messages.json) for list_messages/get_message and
// RECORDS get_message calls to $ACTION_LOG (so an over-fetch — triage stealing
// a reply via read-on-fetch — is an assertable failure). No network.
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { z } from 'zod';
import { readFileSync, appendFileSync } from 'node:fs';

const FIX = process.env.FIXTURE_DIR;
const LOG = process.env.ACTION_LOG;
const log = (line) => appendFileSync(LOG, line + '\n');
const messages = JSON.parse(readFileSync(`${FIX}/messages.json`, 'utf8'));
const summary = (m) => ({
  message_id: m.message_id, conversation_id: m.conversation_id,
  header_from: m.header_from, verified_domain: m.verified_domain, subject: m.subject,
});

const server = new McpServer({ name: 'e2a-stub', version: '0' });

server.registerTool('list_messages',
  { description: 'list messages (summaries; does not mark read)', inputSchema: {
      direction: z.string().optional(), read_status: z.string().optional(),
      sort: z.string().optional(), conversation_id: z.string().optional(), limit: z.number().optional() } },
  async (a) => {
    log(`mcp list_messages ${JSON.stringify(a)}`);
    let out = messages.filter((m) => m.direction === (a.direction || 'inbound'));
    if (a.read_status === 'unread') out = out.filter((m) => m.read !== true);
    if (a.conversation_id) out = out.filter((m) => m.conversation_id === a.conversation_id);
    return { content: [{ type: 'text', text: JSON.stringify(out.map(summary)) }] };
  });

server.registerTool('get_message',
  { description: 'get one message (full body; marks it read on fetch)', inputSchema: { message_id: z.string() } },
  async ({ message_id }) => {
    log(`mcp get_message ${message_id}`); // the read-on-fetch we assert against
    const m = messages.find((x) => x.message_id === message_id) || {};
    return { content: [{ type: 'text', text: JSON.stringify(m) }] };
  });

server.registerTool('get_conversation',
  { description: 'get a thread', inputSchema: { conversation_id: z.string() } },
  async ({ conversation_id }) => {
    log(`mcp get_conversation ${conversation_id}`);
    const ms = messages.filter((m) => m.conversation_id === conversation_id);
    return { content: [{ type: 'text', text: JSON.stringify({ conversation_id, messages: ms }) }] };
  });

await server.connect(new StdioServerTransport());
