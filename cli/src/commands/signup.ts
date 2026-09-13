import { E2AClient, signupAgent } from "@e2a/sdk/v1";
import { loadConfig } from "../config.js";
import { EXIT, fail } from "../exit.js";

export interface SignupCreateOptions {
  humanEmail?: string;
  displayName?: string;
  noteToHuman?: string;
  harness?: string;
  currentApiKey?: string;
  json?: boolean;
}

export interface SignupVerifyOptions {
  apiKey?: string;
  code?: string;
  reviewOutbound?: boolean;
  json?: boolean;
}

export async function signupCreate(opts: SignupCreateOptions): Promise<void> {
  if (!opts.humanEmail || !opts.displayName) {
    fail(
      EXIT.USAGE,
      "usage: e2a signup create --human-email <email> --display-name <name> [--note <text>] [--harness <name>] [--current-api-key <key>] [--json]",
    );
  }
  const config = loadConfig();
  const result = await signupAgent(
    {
      humanEmail: opts.humanEmail,
      displayName: opts.displayName,
      noteToHuman: opts.noteToHuman,
      harness: opts.harness,
      ...(opts.currentApiKey ? { currentApiKey: opts.currentApiKey } : {}),
    },
    { baseUrl: config.api_url },
  );
  if (opts.json) {
    process.stdout.write(JSON.stringify(result) + "\n");
    return;
  }
  process.stdout.write(`inbox:   ${result.inbox}\n`);
  process.stdout.write(`api_key: ${result.apiKey}\n`);
  process.stdout.write(`status:  ${result.status}\n`);
  process.stdout.write(`verification sent to ${result.humanEmail}\n`);
}

export async function signupVerify(opts: SignupVerifyOptions): Promise<void> {
  if (!opts.apiKey || !opts.code) {
    fail(
      EXIT.USAGE,
      "usage: e2a signup verify --api-key <key> --code <six-digits> [--review-outbound] [--json]",
    );
  }
  const config = loadConfig();
  const client = new E2AClient({ apiKey: opts.apiKey, baseUrl: config.api_url });
  const result = await client.agentSignup.verify({
    code: opts.code,
    reviewOutbound: opts.reviewOutbound,
  });
  if (opts.json) {
    process.stdout.write(JSON.stringify(result) + "\n");
    return;
  }
  process.stdout.write(`inbox:  ${result.inbox}\n`);
  process.stdout.write(`status: ${result.status}\n`);
  process.stdout.write(`review_outbound: ${result.reviewOutbound}\n`);
}
