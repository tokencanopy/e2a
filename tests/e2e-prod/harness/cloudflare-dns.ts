import { DnsDeleteError } from "./domain-fixture-cleanup.ts";

export interface CloudflareDnsRecordRef {
  type: string;
  name: string;
  id?: string;
	content?: string;
	comment?: string;
}

export interface CloudflareDnsRecordInput {
  type: string;
  name: string;
  content: string;
  priority?: number;
}

interface CloudflareEnvelope<T> {
  success: boolean;
  result?: T;
  errors?: unknown;
}

type Fetch = typeof fetch;
const REQUEST_TIMEOUT_MS = 15_000;

export function cloudflareFixtureComment(suite: string, domain: string): string {
  return `e2a conformance ${suite} ${domain} (temporary)`;
}

/**
 * Minimal Cloudflare DNS adapter for temporary conformance records.
 *
 * create() records the deterministic type/name descriptor before the POST. If
 * Cloudflare commits but the response is lost or malformed, teardown can list
 * the exact name and still recover the record ID. delete() validates both HTTP
 * status and Cloudflare's success envelope; a 2xx {success:false} is not green.
 */
export class CloudflareDnsClient {
  private readonly api = "https://api.cloudflare.com/client/v4";
	private readonly zone: string;
	private readonly token: string;
	private readonly fetchImpl: Fetch;

  constructor(zone: string, token: string, fetchImpl: Fetch = fetch) {
		this.zone = zone;
		this.token = token;
		this.fetchImpl = fetchImpl;
	}

  async create(
    rec: CloudflareDnsRecordInput,
    tracked: CloudflareDnsRecordRef[],
    comment: string,
  ): Promise<void> {
    const ref: CloudflareDnsRecordRef = { type: rec.type, name: rec.name, content: rec.content, comment };
    tracked.push(ref); // before POST: ambiguous commits remain discoverable
		let res: Response;
		try {
			res = await this.fetchImpl(`${this.api}/zones/${this.zone}/dns_records`, {
				method: "POST",
				headers: this.headers(true),
				body: JSON.stringify({ ...rec, ttl: 60, comment }),
				signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
			});
		} catch (error) {
			throw error; // ambiguous: keep descriptor for exact reconciliation
		}
		let envelope: CloudflareEnvelope<{ id: string }>;
		try {
			envelope = await this.readEnvelope<{ id: string }>(res, `${rec.type} ${rec.name} create`);
		} catch (error) {
			if (res.status >= 400 && res.status < 500 && res.status !== 408 && res.status !== 429) {
				const index = tracked.indexOf(ref);
				if (index >= 0) tracked.splice(index, 1);
			}
			throw error;
		}
    if (!res.ok || !envelope.success || !envelope.result?.id) {
			// A parsed, non-retryable 4xx definitively rejected the create. Disarm
			// this descriptor so teardown cannot delete a pre-existing exact-name
			// record that the run never created.
			if (res.status >= 400 && res.status < 500 && res.status !== 408 && res.status !== 429) {
				const index = tracked.indexOf(ref);
				if (index >= 0) tracked.splice(index, 1);
			}
      throw new Error(`CF ${rec.type} ${rec.name} create failed HTTP ${res.status}: ${JSON.stringify(envelope.errors)}`);
    }
    ref.id = envelope.result.id;
  }

  async delete(ref: CloudflareDnsRecordRef): Promise<void> {
    const ids = ref.id ? [ref.id] : await this.findIds(ref);
    for (const id of ids) await this.deleteId(id);
  }

  /**
   * Verification-only counterpart to delete(): true iff at least one record
   * still matches this ref's type+name (and comment/content, same precedence
   * as findIds()). Callers that already trust a 2xx delete response should
   * NOT need this — it exists for callers that must not trust it, e.g. a
   * teardown step that asserts DNS is actually gone rather than assuming a
   * prior delete() call worked. Never throws DnsDeleteError on zero matches
   * (that is the expected, successful outcome here, not a transient miss).
   */
  async exists(ref: CloudflareDnsRecordRef): Promise<boolean> {
    const query = new URLSearchParams({ type: ref.type, name: ref.name, per_page: "100" });
    const res = await this.fetchImpl(`${this.api}/zones/${this.zone}/dns_records?${query}`, {
      headers: this.headers(false),
      signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
    });
    const envelope = await this.readEnvelope<Array<{ id: string; content?: string; comment?: string }>>(
      res,
      `${ref.type} ${ref.name} exists-check`,
    );
    if (!res.ok || !envelope.success || !Array.isArray(envelope.result)) {
      throw this.httpError(`${ref.type} ${ref.name} exists-check`, res.status, envelope.errors);
    }
    return envelope.result.some((record) => {
      if (ref.comment !== undefined) return record.comment === ref.comment;
      return ref.content === undefined || record.content === ref.content;
    });
  }

  private async findIds(ref: CloudflareDnsRecordRef): Promise<string[]> {
    const query = new URLSearchParams({ type: ref.type, name: ref.name, per_page: "100" });
    const res = await this.fetchImpl(`${this.api}/zones/${this.zone}/dns_records?${query}`, {
      headers: this.headers(false),
		signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
    });
    const envelope = await this.readEnvelope<Array<{ id: string }>>(res, `${ref.type} ${ref.name} lookup`);
    if (!res.ok || !envelope.success || !Array.isArray(envelope.result)) {
      throw this.httpError(`${ref.type} ${ref.name} lookup`, res.status, envelope.errors);
    }
		const matches = envelope.result.filter((record) => {
			const candidate = record as { id: string; content?: string; comment?: string };
			// The per-run comment is the ownership marker. Prefer it over content:
			// Cloudflare may canonicalize MX/TXT content between create and list.
			// Content remains the fallback for legacy descriptors without comments.
			if (ref.comment !== undefined) return candidate.comment === ref.comment;
			return ref.content === undefined || candidate.content === ref.content;
		});
		const ids = matches.map((record) => record.id).filter(Boolean);
		if (ids.length === 0) {
			throw new DnsDeleteError(`CF ${ref.type} ${ref.name} lookup has not found the ambiguously-created record yet`, true);
		}
		return ids;
  }

  private async deleteId(id: string): Promise<void> {
    const res = await this.fetchImpl(`${this.api}/zones/${this.zone}/dns_records/${id}`, {
      method: "DELETE",
      headers: this.headers(false),
		signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
    });
    if (res.status === 404) return;
    const envelope = await this.readEnvelope<unknown>(res, `record ${id} delete`);
    if (!res.ok || !envelope.success) {
      throw this.httpError(`record ${id} delete`, res.status, envelope.errors);
    }
  }

  private async readEnvelope<T>(res: Response, operation: string): Promise<CloudflareEnvelope<T>> {
    try {
      return (await res.json()) as CloudflareEnvelope<T>;
    } catch (error) {
      throw new DnsDeleteError(
        `CF ${operation} returned invalid JSON after HTTP ${res.status}: ${error instanceof Error ? error.message : String(error)}`,
        res.status === 408 || res.status === 429 || res.status >= 500 || res.ok,
      );
    }
  }

  private httpError(operation: string, status: number, errors: unknown): DnsDeleteError {
    return new DnsDeleteError(
      `CF ${operation} failed HTTP ${status}: ${JSON.stringify(errors)}`,
      status === 408 || status === 429 || status >= 500,
    );
  }

  private headers(json: boolean): Record<string, string> {
    return {
      Authorization: `Bearer ${this.token}`,
      ...(json ? { "Content-Type": "application/json" } : {}),
    };
  }
}
