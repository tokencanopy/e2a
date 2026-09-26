import { createInterface } from "node:readline/promises";
import { createClient } from "../sdk.js";
import { saveConfig } from "../config.js";
import { EXIT, fail } from "../exit.js";

export interface AccountDeleteOptions {
  /** Erase the account and all its data immediately instead of trashing it. Irreversible. */
  permanent?: boolean;
  /** Skip the interactive confirmation prompt (required when stdin isn't a TTY). */
  yes?: boolean;
  json?: boolean;
}

export const ACCOUNT_DELETE_USAGE = "usage: e2a account delete [--permanent] [--yes] [--json]";

/**
 * Ask the operator to type the account's own email address before an
 * account-destroying request goes out. Every other destructive command in
 * this CLI (`keys delete`, `contacts delete`, …) treats the typed subcommand
 * itself as the confirmation — the SDK supplies the raw API's
 * `?confirm=DELETE` guard — because each one addresses a single child
 * resource that is either reversible or bounded in scope. Deleting the
 * ACCOUNT revokes the very credential running this command and, with
 * `--permanent`, is irreversible, so it gets its own interactive gate:
 * `--yes` skips it for scripts, and a non-TTY stdin without `--yes` refuses
 * outright rather than hanging on EOF.
 */
async function confirmByTypingEmail(expected: string): Promise<boolean> {
  const rl = createInterface({ input: process.stdin, output: process.stdout });
  try {
    const answer = await rl.question(
      `Type your account email (${expected}) to confirm account deletion: `,
    );
    return answer.trim() === expected;
  } finally {
    rl.close();
  }
}

export async function accountDelete(opts: AccountDeleteOptions): Promise<void> {
  const client = createClient();

  if (!opts.yes) {
    if (!process.stdin.isTTY) {
      fail(
        EXIT.USAGE,
        "refusing to delete the account without confirmation: stdin is not a TTY — pass --yes to confirm non-interactively",
      );
    }
    const account = await client.account.get();
    const confirmed = await confirmByTypingEmail(account.user.email);
    if (!confirmed) {
      fail(EXIT.USAGE, "confirmation did not match — account NOT deleted");
    }
  }

  // `opts.permanent` is a real `false` (not `undefined`) when the flag is
  // simply absent — the arg parser always supplies a boolean. Normalize to
  // `undefined` so the trash path omits `permanent` from the wire entirely,
  // matching the SDK's own default-omits-the-param behavior and every other
  // trash/permanent pair in this codebase (agents.delete, messages.delete).
  const result = await client.account.delete({ permanent: opts.permanent || undefined });

  // The account's own API key is dead the instant this returns — trash mode
  // revokes every key/grant/session immediately, same as permanent erasure.
  // Clear it locally (mirrors what `e2a login` writes, in reverse) so the
  // next command fails fast with the documented "not authenticated" message
  // instead of a confusing 401 from the server.
  saveConfig({ api_key: "", key_scope: "" });

  if (opts.json) {
    process.stdout.write(JSON.stringify(result) + "\n");
    return;
  }

  if (result.mode === "permanent") {
    process.stdout.write("Account permanently deleted. This cannot be undone.\n");
  } else {
    process.stdout.write("Account moved to the trash.\n");
    if (result.purgeAfter) {
      process.stdout.write(
        `It will be purged permanently after ${result.purgeAfter.toISOString()} unless restored.\n`,
      );
    }
    process.stdout.write(
      "Sign in to the dashboard before then to restore it — API keys stay revoked and custom domains must be re-verified.\n",
    );
  }
  process.stderr.write(
    "The API key used for this command is now revoked; local config has been cleared.\n",
  );
}
