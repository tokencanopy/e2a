// server.mjs — the submit_feedback email-bridge MCP server.
//
// Exposes two tools to a calling agent:
//   submit_feedback(kind, title, body, contact?) -> { id, status }
//   feedback_status(id)                           -> { id, status, replies, last_update }
//
// It drops a structured feedback email into the support mailbox (the SAME
// intake the triage lane drains) and reads back the thread it owns for
// status. Pure logic lives in bridge.mjs (unit-tested); this file is the MCP
// + e2a-REST wiring, verified at install (`npm install && node server.mjs`).
//
// Env:
//   E2A_API_URL              e2a REST base (e.g. https://api.e2a.dev)
//   E2A_API_KEY              the BRIDGE's agent-scoped key (its own identity)
//   FEEDBACK_INTAKE_ADDRESS  the bridge's e2a agent address (the From)
//   SUPPORT_ADDRESS          where feedback is delivered (the triage mailbox)
//   FEEDBACK_RATE_PER_HOUR   default 20 (per-process bound; the host/e2a is
//                            the durable limiter)
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { z } from 'zod';
import { validateFeedback, composeFeedbackEmail, statusFromThread, isValidFeedbackId, KINDS } from './bridge.mjs';

const API = process.env.E2A_API_URL;
const KEY = process.env.E2A_API_KEY;
const FROM = process.env.FEEDBACK_INTAKE_ADDRESS;
const SUPPORT = process.env.SUPPORT_ADDRESS;
const RATE = Number(process.env.FEEDBACK_RATE_PER_HOUR || 20);
for (const [k, v] of Object.entries({ E2A_API_URL: API, E2A_API_KEY: KEY, FEEDBACK_INTAKE_ADDRESS: FROM, SUPPORT_ADDRESS: SUPPORT })) {
  if (!v) throw new Error(`submit-feedback bridge: ${k} is required`);
}

async function e2a(method, path, body) {
  const res = await fetch(`${API}${path}`, {
    method,
    headers: { Authorization: `Bearer ${KEY}`, 'Content-Type': 'application/json' },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  if (!res.ok) throw new Error(`e2a ${method} ${path} -> ${res.status}: ${text.slice(0, 300)}`);
  return text ? JSON.parse(text) : {};
}

// Per-process, per-window rate caps — a backstop, not the durable limiter.
// Both submit AND status are gated (status enumeration must not be free).
function limiter(max) {
  const hits = [];
  return () => {
    const now = Date.now();
    while (hits.length && now - hits[0] > 3_600_000) hits.shift();
    if (hits.length >= max) return false;
    hits.push(now);
    return true;
  };
}
const submitOk = limiter(RATE);
const statusOk = limiter(Number(process.env.FEEDBACK_STATUS_RATE_PER_HOUR || 120));

const server = new McpServer({ name: 'submit-feedback', version: '0.1.0' });

server.registerTool(
  'submit_feedback',
  {
    description:
      'File product feedback or a bug from inside this session. Files into the project\'s support queue; returns an id to poll with feedback_status. Does not block on review.',
    inputSchema: {
      kind: z.enum(KINDS),
      title: z.string().max(200),
      body: z.string().max(20000),
      contact: z.boolean().optional(),
    },
  },
  async ({ kind, title, body }) => {
    // Validate BEFORE charging a rate slot.
    const v = validateFeedback({ kind, title, body });
    if (!v.ok) return { content: [{ type: 'text', text: v.error }], isError: true };
    if (!submitOk()) return { content: [{ type: 'text', text: 'RATE_LIMITED: too many feedback submissions this hour' }], isError: true };
    const { subject, text } = composeFeedbackEmail({ kind, title, body });
    // FROM the bridge's identity, TO the support mailbox. No caller-supplied
    // recipient or contact address is ever used (spoof/spam vector).
    let msg;
    try {
      msg = await e2a('POST', `/v1/agents/${FROM}/messages`, { to: [SUPPORT], subject, body: text });
    } catch {
      // Don't surface e2a's raw error (it would disclose the intake address).
      return { content: [{ type: 'text', text: 'UNAVAILABLE: could not file feedback right now — try again' }], isError: true };
    }
    const id = msg.conversation_id || msg.id;
    return { content: [{ type: 'text', text: JSON.stringify({ id, status: 'received' }) }] };
  },
);

server.registerTool(
  'feedback_status',
  {
    description: 'Check the status of feedback you filed (by the id submit_feedback returned).',
    inputSchema: { id: z.string() },
  },
  async ({ id }) => {
    // Reject non-conv ids BEFORE the fetch (.`/`..` would reach unintended
    // same-host endpoints), and rate-gate reads so an id space can't be
    // brute-force enumerated.
    if (!isValidFeedbackId(id)) return { content: [{ type: 'text', text: 'NOT_FOUND: no feedback with that id' }], isError: true };
    if (!statusOk()) return { content: [{ type: 'text', text: 'RATE_LIMITED: too many status checks this hour' }], isError: true };
    let convo;
    try {
      convo = await e2a('GET', `/v1/agents/${FROM}/conversations/${encodeURIComponent(id)}`);
    } catch {
      return { content: [{ type: 'text', text: 'NOT_FOUND: no feedback with that id' }], isError: true };
    }
    const s = statusFromThread(convo.messages || []);
    return { content: [{ type: 'text', text: JSON.stringify({ id, ...s }) }] };
  },
);

await server.connect(new StdioServerTransport());
