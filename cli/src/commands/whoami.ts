import { createClient } from "../sdk.js";
import { loadConfig } from "../config.js";

export interface WhoamiOptions {
  json?: boolean;
}

// `whoami` is the preflight for scripts: one call that answers "is this key
// valid, what scope is it, and which inbox is it bound to". Works for both
// account- and agent-scoped keys (GET /v1/account is scope-aware).
export async function whoami(opts: WhoamiOptions): Promise<void> {
  const client = createClient();
  const account = await client.account.get();

  if (opts.json) {
    process.stdout.write(JSON.stringify(account) + "\n");
    return;
  }

  process.stdout.write(`user:  ${account.user.email} (${account.user.id})\n`);
  process.stdout.write(`scope: ${account.scope}\n`);
  if (account.agentEmail) {
    process.stdout.write(`agent: ${account.agentEmail}\n`);
  } else {
    // Account-scoped keys aren't bound to an inbox — show what send/reply
    // will actually use, so the preflight answers "which inbox am I?".
    const agentEmail = loadConfig().agent_email;
    process.stdout.write(
      agentEmail
        ? `agent: ${agentEmail} (default from config/E2A_AGENT_EMAIL)\n`
        : "agent: (none set — use --agent, E2A_AGENT_EMAIL, or e2a config set agent_email)\n",
    );
  }
  process.stdout.write(`plan:  ${account.planCode}\n`);
  process.stdout.write(
    `usage: ${account.usage.agents}/${account.limits.maxAgents} agents, ` +
      `${account.usage.messagesMonth}/${account.limits.maxMessagesMonth} messages this month\n`,
  );
  // Additive, optional: only ever present right after a dashboard restore
  // from the trash, so most accounts print nothing new here.
  if (account.restoredAt) {
    process.stdout.write(`restored: ${account.restoredAt.toISOString()} (from trash)\n`);
  }

  // Beta, additive: `sending_access` is omitted entirely on a deployment that
  // doesn't run this control, so say nothing rather than printing a
  // misleading "unrestricted". Only surface a line when this account is
  // ACTUALLY restricted right now (enforced, and neither grant applies) —
  // an unrestricted or already-approved account gets no new noise here.
  const access = account.sendingAccess;
  if (access && access.enforcementApplies && !access.sharedExternalApproved && !access.paidExternalSendingEntitled) {
    process.stdout.write(
      "External sending: restricted (send to your verified account email and agent inboxes in " +
        "this account; request approval with: e2a sending-access request --use-case <text> " +
        "--recipients <text> --volume <n>)\n",
    );
  }
}
