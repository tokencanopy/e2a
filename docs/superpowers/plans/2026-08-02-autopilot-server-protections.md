# Autopilot Server Protection Primitives Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add server-enforced authenticated inbound, immutable required owner CC, and opaque protection revisions needed by policy-first autopilot.

**Architecture:** Extend the existing protection record rather than creating an autopilot-specific policy service. The identity store remains authoritative; every protection replacement advances one opaque revision in the same transaction. Inbound admission and outbound recipient composition consume the stored fields at their existing shared enforcement points, while an agent-safe endpoint exposes only the revision.

**Tech Stack:** Go 1.25+, PostgreSQL migrations, Huma/OpenAPI 3.1, generated TypeScript and Python SDKs, TypeScript CLI/MCP, Next.js 16/React 19 dashboard, Go/Node/Vitest tests.

## Global Constraints

- `/v1` is GA: changes are additive; run `make generate` and commit the OpenAPI plus generated TypeScript/Python trees after handler changes.
- Existing agents retain current behavior: `require_authenticated` defaults to `false`, `required_cc` defaults to `[]`.
- Protection revision is opaque, changes after every successful complete protection replacement, and is never accepted from a client request.
- Account scope may read and replace the full posture; an agent-scoped credential may read only the revision for its bound inbox.
- Required CC is normalized, deduplicated case-insensitively across To/CC/BCC, and cannot be removed by agent input or human review overrides.
- Exact-address and domain inbound rules continue to require aligned DMARC; authenticated-open additionally holds unresolved or DMARC-failing mail.
- Use only synthetic addresses and content (`example.test`, `.invalid`) in this public repository.

## File Map

- Create `migrations/095_autopilot_protection.sql`: durable columns and database constraints.
- Modify `internal/identity/store.go`, `internal/identity/protection.go`, `internal/identity/user_data_rights.go`: persistence, validation, replacement, export.
- Modify `internal/inboundpolicy/policy.go`, `internal/inboundscreen/inboundscreen.go`, `internal/relay/server.go`: authenticated-open admission.
- Modify `internal/httpapi/outbound.go`, `internal/agent/screening.go`, `internal/agent/api.go`, `internal/agent/hitl_api.go`, `internal/agent/hitl_magic_api.go`: canonical required-CC merge across initial delivery and held-message approval.
- Modify `internal/agent/hitl_api.go`, `internal/hitlworker/worker.go`, `internal/httpapi/messages.go`: inbound-review wake signal and agent-visible release marker.
- Modify `internal/httpapi/protection.go`: public protection contract and revision-only endpoint.
- Regenerate `api/openapi.yaml`, `sdks/typescript/src/generated/`, `sdks/python/src/e2a/generated/`.
- Modify hand-written client surfaces in `sdks/typescript/src/v1/client.ts`, `sdks/python/src/e2a/v1/client.py`, `cli/src/commands/protection.ts`, `cli/src/bin/e2a.ts`, `mcp/src/client.ts`, `mcp/src/tools/agents.ts`, and dashboard protection files.

---

### Task 1: Persist and Validate the Extended Protection Posture

**Files:**
- Create: `migrations/095_autopilot_protection.sql`
- Modify: `internal/identity/store.go`
- Modify: `internal/identity/protection.go`
- Modify: `internal/identity/user_data_rights.go`
- Test: `internal/identity/protection_test.go`
- Test: `internal/identity/user_data_rights_test.go`

**Interfaces:**
- Produces: `AgentIdentity.InboundRequireAuthenticated bool`, `AgentIdentity.OutboundRequiredCC []string`, `AgentIdentity.ProtectionRevision string`.
- Produces: `ProtectionConfig.InboundRequireAuthenticated bool`, `ProtectionConfig.OutboundRequiredCC []string`.
- Produces: `func NormalizeRequiredCC(values []string) ([]string, error)` returning canonical lowercase mailboxes in first-seen order.
- Produces: `func NewProtectionRevision() string` returning an opaque `prv_`-prefixed identifier.
- Produces: `Store.UpdateAgentProtection` atomically replacing the full posture and advancing `ProtectionRevision`.

- [ ] **Step 1: Add failing validation and mutation tests**

Add table tests that accept normalized unique addresses, reject malformed addresses and more than 50 required CC entries, and prove two successful replacements produce distinct `prv_` revisions while a rejected replacement preserves the prior revision. Use inputs such as:

```go
cfg := identity.ProtectionConfig{
    InboundPolicy:               "open",
    InboundRequireAuthenticated: true,
    OutboundPolicy:              "review",
    OutboundRequiredCC:          []string{"Owner@Example.Test", "owner@example.test"},
}
```

- [ ] **Step 2: Run the focused tests and verify failure**

Run: `go test ./internal/identity -run 'Test(ValidateProtectionConfig|UpdateAgentProtection|UserDataExportProtection)' -count=1`

Expected: FAIL because the new fields, normalization, and revision do not exist.

- [ ] **Step 3: Add the migration and identity fields**

Use database defaults and a bounded array check:

```sql
ALTER TABLE agent_identities
  ADD COLUMN inbound_require_authenticated BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN outbound_required_cc TEXT[] NOT NULL DEFAULT '{}',
  ADD COLUMN protection_revision TEXT NOT NULL
    DEFAULT ('prv_' || encode(gen_random_bytes(16), 'hex')),
  ADD CONSTRAINT agent_outbound_required_cc_limit
    CHECK (cardinality(outbound_required_cc) <= 50),
  ADD CONSTRAINT agent_protection_revision_shape
    CHECK (protection_revision ~ '^prv_[0-9a-f]{32}$');
```

Add the fields to both agent projections/scanners in `store.go` and the user-data export. `NormalizeRequiredCC` parses each value with `net/mail`, rejects display names and non-mailbox syntax, lowercases the complete mailbox, and deduplicates it in first-seen order. `ValidateProtectionConfig` calls the helper for validation; `UpdateAgentProtection` writes the returned canonical slice. Because the export's agent record shape changes, bump `UserExport.SchemaVersion` from `"4"` to `"5"` and update its doc/test to state that v5 adds authenticated-inbound, required-CC, and protection-revision fields.

- [ ] **Step 4: Advance the revision inside the replacement transaction**

Implement:

```go
func NewProtectionRevision() string {
    return "prv_" + generateID()
}
```

Set the new revision in the same `UPDATE` as every protection field, then reload the row inside the transaction. Do not advance on validation or database failure.

- [ ] **Step 5: Re-run identity tests**

Run: `go test ./internal/identity -run 'Test(ValidateProtectionConfig|UpdateAgentProtection|UserDataExportProtection)' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add migrations/095_autopilot_protection.sql internal/identity/store.go internal/identity/protection.go internal/identity/protection_test.go internal/identity/user_data_rights.go internal/identity/user_data_rights_test.go
git commit -m "feat: persist autopilot protection controls"
```

### Task 2: Enforce Authenticated-Open Inbound Admission

**Files:**
- Modify: `internal/inboundpolicy/policy.go`
- Modify: `internal/inboundscreen/inboundscreen.go`
- Modify: `internal/relay/server.go`
- Test: `internal/inboundpolicy/policy_test.go`
- Test: `internal/inboundscreen/inboundscreen_test.go`
- Test: relevant inbound cases in `internal/relay/server_test.go`

**Interfaces:**
- Consumes: `AgentIdentity.InboundRequireAuthenticated` from Task 1.
- Produces: `func EvaluateIngestion(policy string, allowlist []string, requireAuthenticated bool, senderEmail string, senderResolvable bool, dmarcStatus string) Decision`.

- [ ] **Step 1: Write the authenticated-open decision matrix tests**

Cover these exact rows:

```text
open,false,unresolved,none => allow
open,true,resolved,pass => allow
open,true,resolved,fail => review
open,true,unresolved,pass => review
allowlist,true,matching address,pass => allow
allowlist,true,matching address,fail => review
domain,true,matching domain,pass => allow
domain,true,nonmatching domain,pass => review
```

Also pin that exact-address matching occurs only after DMARC alignment and does not inspect `Reply-To`.

- [ ] **Step 2: Run the focused policy tests**

Run: `go test ./internal/inboundpolicy ./internal/inboundscreen -count=1`

Expected: FAIL at the changed signature and authenticated-open rows.

- [ ] **Step 3: Implement the composable flag and update both callers**

For `open`, return the configured review decision when `requireAuthenticated && (!senderResolvable || dmarcStatus != "pass")`; otherwise preserve current behavior. Pass `agent.InboundRequireAuthenticated` from both the relay and loopback screening path.

- [ ] **Step 4: Run inbound policy and relay tests**

Run: `go test ./internal/inboundpolicy ./internal/inboundscreen ./internal/relay -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/inboundpolicy internal/inboundscreen internal/relay
git commit -m "feat: require authenticated inbound when configured"
```

### Task 3: Add Canonical Required-CC Composition

**Files:**
- Modify: `internal/agent/screening.go`
- Modify: `internal/agent/api.go`
- Modify: `internal/httpapi/outbound.go`
- Test: `internal/agent/required_cc_test.go`
- Test: `internal/agent/outbound_suppression_guard_test.go`
- Test: `internal/httpapi/outbound_recipient_cap_test.go`

**Interfaces:**
- Consumes: `AgentIdentity.OutboundRequiredCC` from Task 1 and `outbound.SendRequest`.
- Produces: `func ApplyRequiredCC(agent *identity.AgentIdentity, req *outbound.SendRequest)`.
- Produces: `const MaxOutboundRecipients = 50` and `func OutboundRecipientCount(req outbound.SendRequest) int` in `internal/agent`, reused by the HTTP and approval paths.

- [ ] **Step 1: Write failing composition tests**

Test a send, reply, and forward; an address already in To, CC, or BCC with different case; multiple required addresses; and recipient-limit/suppression checks after insertion. The core assertion is:

```go
req := outbound.SendRequest{To: []string{"customer@example.test"}, CC: []string{"OWNER@example.test"}}
ApplyRequiredCC(&identity.AgentIdentity{OutboundRequiredCC: []string{"owner@example.test", "audit@example.test"}}, &req)
assert.Equal(t, []string{"OWNER@example.test", "audit@example.test"}, req.CC)
```

- [ ] **Step 2: Run tests and verify failure**

Run: `go test ./internal/agent ./internal/httpapi -run 'Test(ApplyRequiredCC|DeliverOutboundRequiredCC|RequiredCCRecipientCap)' -count=1`

Expected: FAIL because `ApplyRequiredCC` is undefined and delivery does not inject recipients.

- [ ] **Step 3: Implement one canonical merge helper**

Build a case-insensitive set across To/CC/BCC using the same normalized mailbox semantics as protection validation. Append only missing required recipients to CC in stored order.

- [ ] **Step 4: Apply it before final HTTP validation and at the core boundary**

Move the canonical limit value to `agent.MaxOutboundRecipients` and have the existing HTTP `recipientCountError` reference it. In `internal/httpapi/outbound.go:deliver`, call `ApplyRequiredCC` immediately after `prepare()` and before the final `recipientCountError` and `agent.ValidateRecipients` calls; this catches a user-supplied 50-recipient request that becomes 51 after owner CC. Also call the idempotent helper at the first line of `DeliverOutbound` as defense in depth for legacy/non-HTTP callers, and reject `OutboundRecipientCount(req) > MaxOutboundRecipients` with `too_many_recipients` before unsubscribe preparation, suppression, screening, hold-preview, self-send, or provider routing.

- [ ] **Step 5: Run outbound tests**

Run: `go test ./internal/agent ./internal/httpapi -run 'Test(ApplyRequiredCC|DeliverOutboundRequiredCC|RequiredCCRecipientCap|DeliverOutbound_AgentSuppression)' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/agent/screening.go internal/agent/api.go internal/agent/required_cc_test.go internal/agent/outbound_suppression_guard_test.go internal/httpapi/outbound.go internal/httpapi/outbound_recipient_cap_test.go
git commit -m "feat: enforce required cc on outbound mail"
```

### Task 4: Preserve Required CC Through Human Approval

**Files:**
- Modify: `internal/agent/hitl_api.go`
- Modify: `internal/agent/hitl_magic_api.go`
- Modify: `internal/hitlworker/worker.go`
- Modify: `internal/httpapi/hitl.go`
- Modify: `internal/httpapi/reviews.go`
- Test: `internal/agent/hitl_required_cc_test.go`
- Test: `internal/hitlworker/required_cc_test.go`
- Test: `internal/httpapi/review_protection_test.go`

**Interfaces:**
- Consumes: `ApplyRequiredCC(agent, *outbound.SendRequest)`, `OutboundRecipientCount`, and `MaxOutboundRecipients` from Task 3.
- Preserves: `ApprovePendingCore` and magic-link approval public behavior, with canonical CC written into the final approved request.

- [ ] **Step 1: Add failing override-removal tests**

Create a held outbound message whose preview includes `owner@example.test`; approve it with overrides that omit the owner or move it into another recipient field. Assert the final submitted request contains the owner exactly once in CC. Repeat through dashboard/API approval, magic-link approval, and TTL auto-approval; include an expired reject-on-expiry hold that remains rejected.

- [ ] **Step 2: Run focused approval tests**

Run: `go test ./internal/agent ./internal/hitlworker ./internal/httpapi -run 'Test.*RequiredCC.*Approval' -count=1`

Expected: FAIL because reviewer edits can currently remove the required recipient.

- [ ] **Step 3: Canonicalize after merging reviewer edits**

In `ApprovePendingCore`, load the agent, merge `ApproveOverrides`, build the final `outbound.SendRequest`, call `ApplyRequiredCC`, reject a final count above `MaxOutboundRecipients`, and copy the canonical To/CC/BCC back into the approval edit record before size, suppression, and async submission checks. Route magic-link approval through the same canonical function instead of duplicating recipient composition; TTL auto-approval must call the same helper before composing its final request.

- [ ] **Step 4: Re-run approval and suppression tests**

Run: `go test ./internal/agent ./internal/hitlworker ./internal/httpapi -run 'Test(ApprovePendingCore|.*RequiredCC.*|.*Suppression.*)' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/hitl_api.go internal/agent/hitl_magic_api.go internal/agent/hitl_required_cc_test.go internal/hitlworker/worker.go internal/hitlworker/required_cc_test.go internal/httpapi/hitl.go internal/httpapi/reviews.go internal/httpapi/review_protection_test.go
git commit -m "fix: retain required cc through review approval"
```

### Task 5: Wake Autopilot After an Inbound Review Release

**Files:**
- Modify: `internal/agent/hitl_api.go`
- Modify: `internal/agent/api.go`
- Modify: `internal/hitlworker/worker.go`
- Modify: `internal/httpapi/messages.go`
- Test: `internal/agent/inbound_review_core_test.go`
- Test: `internal/hitlworker/events_test.go`
- Test: `internal/httpapi/messages_test.go`

**Interfaces:**
- Produces WebSocket event `email.review_approved` for an inbound human/TTL release, using the exact durable event envelope.
- Produces beta response field `inbound_review_status` on `MessageView` and `MessageSummaryView`, omitted except for inbound `review_approved` or `review_expired_approved` rows.

- [ ] **Step 1: Write failing human-release and TTL-release wake tests**

Wire a fake WebSocket hub, release a held inbound message, and assert one `email.review_approved` envelope with `direction: "inbound"` and the exact message/agent IDs reaches the bound inbox. Repeat for TTL release. Assert the bytes equal the envelope persisted for webhook/event delivery; no reconstructed alternate shape is allowed.

- [ ] **Step 2: Write failing message-marker tests**

Assert the bound agent can GET and list a released inbound message and see `inbound_review_status: "review_approved"` (or `review_expired_approved`), while ordinary authenticated inbound omits the field. The response must not reveal reviewer user IDs, review notes, protection configuration, or account data.

- [ ] **Step 3: Run focused tests**

Run: `go test ./internal/agent ./internal/hitlworker ./internal/httpapi -run 'Test.*Inbound.*Review.*(WebSocket|Marker|Release|Approved)' -count=1`

Expected: FAIL because review releases currently publish only to the event outbox and inbound message views hide the release state.

- [ ] **Step 4: Push the stored review-approved envelope**

After the release transition and durable event publish, load the canonical envelope with `GetEventEnvelope(ctx, messageID, webhookpub.EventEmailReviewApproved)` and send it through the existing agent-bound WebSocket hub when connected. A missing envelope leaves the already-committed release intact; reconciliation uses the marker.

- [ ] **Step 5: Add the narrow inbound marker**

Add the same field to both detail and summary views:

```go
InboundReviewStatus string `json:"inbound_review_status,omitempty" doc:"Beta: present only when a held inbound message was released by human review or expiry. Known values: review_approved, review_expired_approved."`
```

Populate it only for inbound rows with one of those two statuses. Keep the existing outbound `review_status` behavior unchanged.

- [ ] **Step 6: Run tests and commit**

Run: `go test ./internal/agent ./internal/hitlworker ./internal/httpapi -run 'Test.*Inbound.*Review.*(WebSocket|Marker|Release|Approved)' -count=1`

Expected: PASS.

```bash
git add internal/agent/api.go internal/agent/hitl_api.go internal/agent/inbound_review_core_test.go internal/hitlworker/worker.go internal/hitlworker/events_test.go internal/httpapi/messages.go internal/httpapi/messages_test.go
git commit -m "feat: wake agents when inbound review releases"
```

### Task 6: Extend the REST Contract and Add Revision-Only Agent Access

**Files:**
- Modify: `internal/httpapi/protection.go`
- Modify: `internal/httpapi/protection_test.go`
- Modify: `internal/httpapi/stability_test.go`
- Regenerate: `api/openapi.yaml`
- Regenerate: `sdks/typescript/src/generated/`
- Regenerate: `sdks/python/src/e2a/generated/`

**Interfaces:**
- Produces wire fields: inbound `require_authenticated: boolean`, outbound `required_cc: string[]`, response-only top-level `revision: string`.
- Produces: `GET /v1/agents/{email}/protection/revision` with `ProtectionRevisionView { revision: string }`.

- [ ] **Step 1: Write failing authorization and schema tests**

Pin that account scope can GET/PUT the full protection object; the bound agent scope gets 403 from full GET/PUT but 200 from revision GET; an agent key bound to a different inbox gets 404/403 under the existing non-enumeration contract. Assert the strict PUT request schema has no revision field and rejects a client-supplied revision.

- [ ] **Step 2: Run HTTP tests**

Run: `go test ./internal/httpapi -run 'Test(Protection|Stability)' -count=1`

Expected: FAIL because the fields and revision operation do not exist.

- [ ] **Step 3: Add distinct inbound and outbound direction request/view types**

Avoid semantically invalid shared fields. Use:

```go
type InboundProtectionDirectionView struct {
    Gate ProtectionGateView `json:"gate"`
    Scan ProtectionScanView `json:"scan"`
    RequireAuthenticated bool `json:"require_authenticated"`
}
type OutboundProtectionDirectionView struct {
    Gate ProtectionGateView `json:"gate"`
    Scan ProtectionScanView `json:"scan"`
    RequiredCC []string `json:"required_cc"`
}
type ProtectionRevisionView struct { Revision string `json:"revision"` }
```

Mirror these as request types without a revision. Register the revision GET using `resolveOwnedAgent`; return only the opaque string.

- [ ] **Step 4: Run HTTP tests and generate clients**

Run: `go test ./internal/httpapi -run 'Test(Protection|Stability)' -count=1`

Expected: PASS.

Run: `make generate`

Expected: `api/openapi.yaml` and both generated SDK trees change only for the additive contract.

- [ ] **Step 5: Run contract gates**

Run: `make spec-check && make generate-sdk-check && make openapi-compat-check`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/httpapi api/openapi.yaml sdks/typescript/src/generated sdks/python/src/e2a/generated
git commit -m "feat: expose autopilot protection contract"
```

### Task 7: Add Client, CLI, MCP, and Dashboard Parity

**Files:**
- Modify: `sdks/typescript/src/v1/client.ts`
- Modify: `sdks/typescript/test/v1/client.test.ts`
- Modify: `sdks/python/src/e2a/v1/client.py`
- Modify: `sdks/python/tests/test_v1_client.py`
- Modify: `cli/src/commands/protection.ts`
- Modify: `cli/src/bin/e2a.ts`
- Modify: `cli/src/__tests__/protection.test.ts`
- Modify: `mcp/src/client.ts`
- Modify: `mcp/src/tools/agents.ts`
- Modify: `mcp/tests/tools.test.ts`
- Modify: `web/src/app/components/onboarding/types.ts`
- Modify: `web/src/app/components/onboarding/api.ts`
- Modify: `web/src/app/(app)/inboxes/_components/ProtectionEditor.tsx`
- Modify: `web/src/app/(app)/inboxes/_components/ProtectionEditor.test.tsx`

**Interfaces:**
- Produces TS/Python `getProtectionRevision(email)`.
- Produces CLI `e2a protection revision <email> --json`.
- Produces CLI `e2a protection replace <email> --file <path> --json`, which parses one strict complete `ProtectionConfigRequest` JSON object.
- Produces MCP read/update schemas for the new protection fields; MCP does not expose revision as a policy-administration shortcut.

- [ ] **Step 1: Add failing surface tests**

Assert SDK route/method mapping, strict CLI file parsing with unknown-key rejection, JSON output, agent-safe revision output, MCP schema fields, and dashboard initialization/save of authenticated inbound plus required CC. Use a temporary JSON policy containing only synthetic addresses.

- [ ] **Step 2: Run the focused test suites**

Run: `npm run build --workspace @e2a/sdk && npm run test:unit --workspace @e2a/sdk -- client.test.ts`

Run: `npm test --workspace @e2a/cli -- protection.test.ts`

Run: `npm test --workspace @e2a/mcp-server -- tools.test.ts`

Run: `npm --prefix web test -- ProtectionEditor.test.tsx`

Expected: FAIL at missing methods, flags, schema fields, and form controls.

- [ ] **Step 3: Implement SDK and CLI methods**

Have `protection replace --file` use `readFile`, `JSON.parse`, explicit allowed-key validation at every object level, then call the existing complete-replacement client method. Never accept partial patches. Print the server-returned full config or revision object under `--json`.

- [ ] **Step 4: Implement MCP and dashboard parity**

Add the two fields to MCP read/update schemas and the web protection model/editor. Show required CC as validated mailbox chips/text rows and authenticated inbound as an explicit toggle with copy explaining aligned DMARC.

- [ ] **Step 5: Re-run all surface tests**

Run: `npm run build --workspace @e2a/sdk && npm run test:unit --workspace @e2a/sdk -- client.test.ts`

Run: `npm test --workspace @e2a/cli -- protection.test.ts`

Run: `npm test --workspace @e2a/mcp-server -- tools.test.ts`

Run: `npm --prefix web test -- ProtectionEditor.test.tsx`

Run: `cd sdks/python && pytest tests/test_v1_client.py -q`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add sdks/typescript/src/v1 sdks/typescript/test/v1/client.test.ts sdks/python/src/e2a/v1 sdks/python/tests cli/src mcp/src mcp/tests web/src
git commit -m "feat: add protection controls to every client surface"
```

### Task 8: Run the Server-Protection Release Gate

**Files:**
- Modify if needed: `docs/api.md`
- Test: all touched backend and client surfaces.

**Interfaces:**
- Verifies all interfaces produced by Tasks 1–7.

- [ ] **Step 1: Document the additive fields and security semantics**

Add the revision endpoint, aligned-DMARC meaning, required-CC immutability, inbound release marker, and review-approved WebSocket wake to `docs/api.md`. Explicitly state that exact From address plus aligned DMARC authenticates the domain use for the message, not the individual human mailbox controller.

- [ ] **Step 2: Run formatters and the complete unit gate**

Run: `gofmt -w internal/identity/store.go internal/identity/protection.go internal/identity/user_data_rights.go internal/inboundpolicy/policy.go internal/inboundscreen/inboundscreen.go internal/relay/server.go internal/agent/screening.go internal/agent/api.go internal/agent/hitl_api.go internal/agent/hitl_magic_api.go internal/hitlworker/worker.go internal/httpapi/messages.go internal/httpapi/outbound.go internal/httpapi/protection.go`

Run: `make test-unit`

Expected: PASS.

- [ ] **Step 3: Run generated-contract gates**

Run: `make spec-check && make generate-sdk-check && make openapi-compat-check`

Expected: PASS with no generated drift and no GA incompatibility.

- [ ] **Step 4: Run all client gates**

Run: `npm run build --workspace @e2a/sdk && npm test --workspace @e2a/sdk && npm test --workspace @e2a/cli && npm test --workspace @e2a/mcp-server && npm --prefix web test`

Run: `cd sdks/python && pytest tests -q`

Expected: PASS.

- [ ] **Step 5: Inspect the final diff and commit documentation**

Run: `git diff --check && git status --short`

Expected: no whitespace errors; only planned files are changed.

```bash
git add docs/api.md
git commit -m "docs: explain autopilot protection guarantees"
```
