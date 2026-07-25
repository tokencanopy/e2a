# Production E2E harness

Run this harness only against the intended production-compatible deployment. Configure:

- `E2A_URL`: API base URL.
- `E2A_API_KEY`: test-account API key.
- `E2A_AGENT_EMAIL`: primary agent owned by that test account.
- `E2E_SINK_EMAIL`: explicit safe test sink. This is required; never use a real agent or user mailbox. CI uses the Amazon SES mailbox simulator.
- `E2A_SITE_URL`: optional site URL override when it cannot be derived from `E2A_URL`.
- `E2A_MCP_URL`: optional MCP endpoint override; defaults to `${E2A_URL}/mcp`.
- `E2A_QUOTA_API_KEY` / `E2A_QUOTA_AGENT_EMAIL`: optional separate STANDARD-class,
  low-cap account for the limit/rate-limit enforcement suite. Without these the
  enforcement suite is skipped, since the main conformance account is
  internal-class and exempt by construction.

The registration rate-limit stress probe is disabled by default. Set `E2E_PROD_STRESS=1` only for an intentional stress run.

Do not commit credentials or print secret values in reports or logs.

## Running

```bash
npm test              # run all suites (suites/*.test.ts)
npm run smoke          # quick smoke check (smoke.ts)
npm run coverage       # run all suites, then the coverage gate
```
