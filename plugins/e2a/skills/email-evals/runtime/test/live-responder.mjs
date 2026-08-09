import { E2AClient } from "@e2a/sdk/v1";

const REQUIRED_ENVIRONMENT = [
  "E2A_EVAL_API_KEY",
  "E2A_EVAL_BASE_URL",
  "E2A_EVAL_ACTOR",
  "E2A_EVAL_TARGET",
];
const SUBJECT = "Question about fictional order ord_example_123";
const RESPONSE = "Refunds are available within 30 days. This is a synthetic policy answer.";
const DEADLINE_MS = 20_000;
const POLL_MS = 1_000;
const SAFE_STATUS = /^[a-z_]{1,32}$/;
const SAFE_MESSAGE_ID = /^msg_[a-f0-9]{32}$/;

function requiredEnvironment() {
  const values = Object.fromEntries(REQUIRED_ENVIRONMENT.map((name) => [name, process.env[name]]));
  if (Object.values(values).some((value) => typeof value !== "string" || value.length === 0)) {
    throw new Error("missing responder configuration");
  }
  return values;
}

const sleep = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

async function main() {
  const environment = requiredEnvironment();
  const client = new E2AClient({
    apiKey: environment.E2A_EVAL_API_KEY,
    baseUrl: environment.E2A_EVAL_BASE_URL,
    maxRetries: 2,
    maxElapsedMs: 10_000,
    timeoutMs: 5_000,
  });
  const deadline = Date.now() + DEADLINE_MS;

  while (Date.now() < deadline) {
    const page = client.messages.list(environment.E2A_EVAL_TARGET, {
      direction: "inbound",
      readStatus: "all",
      from_: environment.E2A_EVAL_ACTOR,
      subjectContains: SUBJECT,
      limit: 20,
    });
    const messages = await page.toArray({ limit: 20 });
    const stimulus = messages.find((message) => message.subject === SUBJECT);
    if (!stimulus) {
      await sleep(POLL_MS);
      continue;
    }

    const result = await client.messages.reply(
      environment.E2A_EVAL_TARGET,
      stimulus.id,
      { text: RESPONSE },
      { idempotencyKey: `email-eval-reply-${stimulus.id}`, wait: "sent" },
    );
    if (!result || !SAFE_STATUS.test(result.status) || !SAFE_MESSAGE_ID.test(result.messageId)) {
      throw new Error("reply returned an unsafe result");
    }
    process.stdout.write(`${JSON.stringify({ status: result.status, message_id: result.messageId })}\n`);
    return;
  }
  throw new Error("responder deadline exceeded");
}

main().catch(() => {
  process.stderr.write("live responder failed\n");
  process.exitCode = 1;
});
