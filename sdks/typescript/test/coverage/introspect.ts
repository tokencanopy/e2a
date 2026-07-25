// Ergonomic-surface coverage gate — denominator via RUNTIME INTROSPECTION.
//
// This is the TS-SDK analogue of tests/e2e-prod/mcp_coverage_gate.py +
// harness/mcp-coverage.ts, which measure the deployed MCP server's *advertised*
// tool catalog rather than grepping mcp/src/tools/*.ts. There is no server to
// ask "what did you advertise" for an SDK — the closest honest equivalent is
// the BUILT, INSTANTIATED client object itself: walk it exactly the way a
// consumer's code would (`client.agents.create`, `client.messages.send`, …)
// and record every callable name that surfaces.
//
// Why not grep src/v1/client.ts for method definitions? Because a regex over
// the source measures the file, not the thing under test — and it already
// undercounts here: a naive grep over client.ts reports ~32 methods while this
// walker reports more (see the count printed by gate.mjs), the same shape of
// mistake already caught once on the MCP side (grep said 58 tools, the server
// advertised 60). The gap is nested/aliased structure a regex can't see:
// sub-resources one level below a top-level resource (`account.suppressions.*`,
// `account.apiKeys.*`), and resources reachable through more than one path
// (`inbound.messages` and `webhooks.messages` are the SAME MessagesResource
// instance as `client.messages` — must not double-count). The walk below is
// RECURSIVE (bounded by MAX_DEPTH, not by "one level") specifically so a
// sub-resource nested under a sub-resource is never silently missed — an
// undercounted denominator is a worse failure than an overcounted one, since
// it lets a real method escape the gate unnoticed rather than merely printing
// a harmless allowlist candidate.
//
// A property being TypeScript `private` is NOT a usable signal here: `private`
// is erased by `tsc` and is fully reachable on the live object (e.g.
// `client.agents.api.getAgentWithHttpInfo(...)` runs today). Concretely,
// EVERY hand-written ergonomic Resource class in client.ts (AgentsResource,
// MessagesResource, …) wraps its internal OpenAPI-Generator transport handle
// in a `private readonly api: Promise<X>Api`, and `client.meta` itself is
// `private readonly meta: PromiseMetaApi` — assigned the raw generated client
// directly, with no wrapper class at all (unlike every other domain, which
// gets an ergonomic Resource wrapper). Both are internal by the SAME
// intention (only reachable via the wrapping class's own public methods:
// `agents.get()`, `client.info()`), and `private` hides neither at runtime.
//
// The runtime-OBSERVABLE signal this walker actually uses is the generated
// layer's own naming convention: OpenAPI-Generator emits one Promise-wrapped
// API class per tag, always named `Promise<Tag>Api` (PromiseAgentsApi,
// PromiseMetaApi, …— see src/v1/generated/types/PromiseAPI.ts). A node whose
// constructor name matches that pattern IS the generated layer, regardless of
// which field name it's reachable through — so it is excluded uniformly,
// whether it's `agents`'s `.api` field or `client`'s own `.meta` field. This
// generalizes past a name-based guess ("skip anything called `api`") to
// something that would also catch a future unwrapped resource the way
// `client.meta` is unwrapped today, without hand-listing it. The generated
// /v1 surface these nodes carry (getInfo, getInfoWithHttpInfo, …) is already
// held accountable transitively by tests/e2e-prod's coverage_gate.py via
// api/openapi.yaml — that is what makes it safe, not just convenient, to
// exclude from THIS gate's denominator.

const MAX_DEPTH = 4;

const GENERATED_API_CLASS_NAME = /^Promise[A-Za-z0-9]*Api$/;

function isPlainObjectLike(value: unknown): value is object {
  return value !== null && typeof value === "object";
}

function constructorName(node: object): string | undefined {
  const proto = Object.getPrototypeOf(node) as object | null;
  return proto?.constructor?.name;
}

/** True for a node that IS the generated OpenAPI-Generator transport layer
 *  (a `Promise<Tag>Api` instance) — internal by construction, regardless of
 *  which property name reaches it. See the module comment for why this is
 *  the runtime-observable signal used instead of TypeScript `private`. */
function isGeneratedApiNode(node: object): boolean {
  const name = constructorName(node);
  return !!name && GENERATED_API_CLASS_NAME.test(name);
}

function ownMethodNames(node: object): string[] {
  const proto = Object.getPrototypeOf(node) as object | null;
  if (!proto || proto === Object.prototype) return [];
  return Object.getOwnPropertyNames(proto).filter((name) => {
    if (name === "constructor") return false;
    const desc = Object.getOwnPropertyDescriptor(proto, name);
    return !!desc && typeof desc.value === "function";
  });
}

function ownObjectProps(node: object): Array<[string, object]> {
  const out: Array<[string, object]> = [];
  for (const [key, value] of Object.entries(node)) {
    if (isPlainObjectLike(value)) out.push([key, value]);
  }
  return out;
}

/**
 * Walk a live `E2AClient` instance and return every ergonomic-surface method
 * id ("agents.create", "account.apiKeys.delete", "info", …), sorted. Pure
 * reflection — makes no network calls, so it's safe to run in a `beforeAll`
 * even offline.
 */
export function walkErgonomicSurface(client: object): string[] {
  const ids: string[] = [];
  const visited = new Set<object>();

  function visit(node: object, path: string, depth: number): void {
    if (visited.has(node)) return;
    visited.add(node);
    if (isGeneratedApiNode(node)) return; // internal — see module comment.
    for (const method of ownMethodNames(node)) {
      ids.push(path ? `${path}.${method}` : method);
    }
    if (depth >= MAX_DEPTH) return;
    for (const [key, value] of ownObjectProps(node)) {
      visit(value, path ? `${path}.${key}` : key, depth + 1);
    }
  }

  visit(client, "", 0);
  return ids.sort();
}
