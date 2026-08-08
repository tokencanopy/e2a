# Email templates (beta)

> **Beta.** Templates are unstable — their shape may change before they are
> declared stable. The canonical contract is [`api/openapi.yaml`](../api/openapi.yaml).

## Using a coding agent?

Templates are a one-time setup your coding agent can do headlessly — copy this prompt:

> Read https://e2a.dev/templates.md and set up e2a email
> templates for this project: browse the starter templates, copy the ones we need via
> `from_starter`, brand them (accent color is marked `<!-- BRAND: accent -->`), and wire
> our transactional sends using `template_alias` + `template_data`. Use the e2a MCP tools
> if connected (otherwise the REST API with `$E2A_API_KEY`), validate each template before
> wiring it in, and finish by listing the templates you created plus the send code you added.

Templates are reusable email sources — a subject, a plain-text body, and an
optional HTML body — stored on your account and **rendered server-side at send
time**. Instead of composing subject/body in your agent code, you reference a
template by alias and pass the variable values:

- `POST /v1/templates` — create (or copy a starter with `from_starter`)
- `GET/PATCH/DELETE /v1/templates/{id}` — manage
- `POST /v1/templates/validate` — dry-run sources + render a preview without persisting
- `GET /v1/starter-templates` / `GET /v1/starter-templates/{alias}` — the read-only starter catalog

The dashboard surface lives at **/templates** (list, edit, starter gallery, and
a rendered preview with an HTML/text tab switch and light/dark toggle).

## Syntax

Template syntax is intentionally minimal — variable interpolation only, no
loops, no conditionals, no partials:

| Form | Behavior |
|---|---|
| `{{variable}}` | Interpolates the value. In the **HTML part** the value is HTML-escaped; in the subject and plain-text parts it is inserted as-is. |
| `{{{variable}}}` | Raw insertion (HTML part): the value is inserted **without escaping**. For pre-rendered HTML fragments only — see the warning below. |

Missing variables render as **empty strings**. Variable names match
`[A-Za-z_][A-Za-z0-9_.]*`-style identifiers (e.g. `order_id`, `items_html`) —
a dot path (e.g. `{{user.name}}`) resolves into a nested object, matching how
`template_data` is passed as nested JSON.

**Reserved-section note:** anything that is not a `{{…}}` / `{{{…}}}` slot is
literal template text and is emitted verbatim — including `{` and `}`
characters in CSS or code samples. Only the double/triple-brace forms are
interpreted; there is no escape sequence and no other directive syntax.

## Sending with a template

Reference the template by its per-account alias (`template_alias`) or id
(`template_id`) — mutually exclusive with literal `subject`/`text`/`html` —
and pass the variables in `template_data`:

```bash
curl -X POST https://api.e2a.dev/v1/agents/billing-bot%40agents.e2a.dev/messages \
  -H "Authorization: Bearer $E2A_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "to": ["customer@example.com"],
    "template_alias": "receipt",
    "template_data": {
      "company_name": "Acme",
      "support_email": "support@acme.com",
      "company_address": "100 Main St, San Francisco, CA 94105",
      "order_id": "ORD-10432",
      "order_date": "July 2, 2026",
      "items_html": "<tr><td style=\"padding:8px 0;\">1x Pro plan (monthly)</td><td style=\"padding:8px 0;\" align=\"right\">$29.00</td></tr>",
      "items_text": "1x Pro plan (monthly) — $29.00",
      "total": "$29.00",
      "receipt_url": "https://app.acme.com/receipts/ORD-10432"
    }
  }'
```

Rendering happens **before** any human-in-the-loop review hold, so reviewers
see the final subject and body.

> **Wire change.** To make room for the template shape, `subject` and `text`
> moved from schema-required to handler-enforced on
> `POST /v1/agents/{email}/messages`. A literal send that omits them now
> returns **400 `invalid_request`** where it previously returned a **422**
> schema-validation error. Sends that include them are unaffected.

## Starter templates

The deployment ships a read-only catalog of pre-built, ISP-friendly starters
(responsive tables, dark-mode support, CAN-SPAM footer). `POST /v1/templates`
with `{"from_starter": "<alias>"}` copies the master **verbatim** into your
library (name/alias default to the starter's and may be overridden); edit the
copy freely afterwards.

| Alias | Purpose | Variables |
|---|---|---|
| `welcome` | Warm, brief welcome with a single primary CTA | `company_name`, `support_email`, `company_address`, `preheader`*, `recipient_name`, `cta_url`, `cta_label` |
| `verify-code` | Terse one-time verification code (selectable monospace chip, no button) | `company_name`, `support_email`, `company_address`, `preheader`*, `code`, `expires_minutes` |
| `password-reset` | Security-neutral password reset with one reset button and expiry notice | `company_name`, `support_email`, `company_address`, `preheader`*, `action_url`, `expires_minutes` |
| `receipt` | Order receipt with a line-item table and hosted-receipt link | `company_name`, `support_email`, `company_address`, `preheader`*, `order_id`, `order_date`, **`items_html`** (raw), `items_text`, `total`, `receipt_url` |
| `agent-status` | Numbers-first status report from an automated agent | `company_name`, `support_email`, `company_address`, `preheader`*, `agent_name`, `run_summary`, **`sections_html`** (raw), `sections_text`, `dashboard_url` |
| `daily-digest` | Recurring daily summary with an unsubscribe link | `company_name`, `support_email`, `company_address`, `preheader`*, `agent_name`, `date`, `headline`, **`sections_html`** (raw), `sections_text`, `dashboard_url`, **`unsubscribe_html`** (raw) |
| `approval-request` | Human-in-the-loop approval request with Approve/Reject buttons and expiry | `company_name`, `support_email`, `company_address`, `preheader`*, `agent_name`, `action_summary`, **`details_html`** (raw), `details_text`, `approve_url`, `reject_url`, `expires_at` |

\* optional; **bold** = raw (`{{{…}}}`) slot. `GET /v1/starter-templates`
returns the authoritative per-variable metadata (required/raw flags,
descriptions, example values usable directly as `template_data`).

## The `{{{items_html}}}` fragment pattern

Several starters take a **raw HTML fragment** for their repeated content (line
items, report sections, key-value rows). The template owns the surrounding
table and styling; your agent supplies only the inner rows, pre-rendered with
inline styles:

```json
{
  "items_html": "<tr><td style=\"padding:8px 0;font-family:'Helvetica Neue',Arial,sans-serif;font-size:14px;color:#1F2430;\">1x Pro plan (monthly)</td><td style=\"padding:8px 0;font-size:14px;color:#1F2430;\" align=\"right\">$29.00</td></tr><tr><td style=\"padding:8px 0;font-size:14px;color:#1F2430;\">1x Extra seat</td><td style=\"padding:8px 0;font-size:14px;color:#1F2430;\" align=\"right\">$10.00</td></tr>",
  "items_text": "1x Pro plan (monthly) — $29.00\n1x Extra seat — $10.00"
}
```

> **Warning — escape user content in raw slots.** `{{{…}}}` inserts the value
> into the HTML part **without escaping**. If any part of the fragment comes
> from user- or third-party-controlled data (product names, memo lines,
> addresses), HTML-escape those substrings *before* building the fragment —
> e.g. a product name `Widget <img src=x onerror=…>` must be inserted as
> `Widget &lt;img src=x onerror=…&gt;`. Never pass untrusted input to a raw
> slot; use the escaped `{{…}}` form wherever a plain string will do. Always
> fill the matching `*_text` variable too, so the plain-text part carries the
> same content.

## Approval-request URLs: **require a confirmation page**

**`approve_url` and `reject_url` MUST land on a page that requires an explicit
human click to take effect — never on a state-changing GET.** Corporate
mail-security scanners (Safe Links, spam filters, previewers) **fetch every
link in an email** before or after delivery. If your approve/reject URL
mutates state directly on GET, a scanner will silently approve or reject the
action with no human involved. Serve a confirmation page at those URLs and
perform the actual approve/reject on a POST from that page's button. (One-time
tokens alone do not save you — the scanner's GET consumes the token.)

## See also

- [`docs/api.md`](api.md) — REST surface overview and conventions
- [`api/openapi.yaml`](../api/openapi.yaml) — the canonical machine-readable contract
