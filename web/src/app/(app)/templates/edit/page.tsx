"use client";

// /templates/edit?id=tpl_… — detail/edit view for one template. Uses a
// query param instead of a path segment because web/ is statically
// exported (next.config.ts) and dynamic segments would require
// generateStaticParams() with every id enumerated at build time — same
// pattern as the per-agent /inboxes/* screens.

import { Suspense, useCallback, useEffect, useState } from "react";
import { useSearchParams } from "next/navigation";
import { PageShell } from "../../../components/loft/PageShell";
import { Button, Chip } from "@e2a/ui";
import { TemplatePreview } from "../_components/TemplatePreview";
import {
  readErrorBody,
  type TemplateView,
  type ValidateTemplateResponse,
} from "../_lib/types";
import { flattenSuggested, nestTestData } from "../_lib/testdata";

const fieldStyle: React.CSSProperties = {
  background: "var(--bg-panel)",
  border: "1px solid var(--border)",
  borderRadius: "var(--r-md)",
  color: "var(--fg)",
};

function LabeledField({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <label className="block">
      <span className="text-[13px] font-medium" style={{ color: "var(--fg)" }}>
        {label}
      </span>
      {hint && (
        <span
          className="block text-[11px] mt-0.5"
          style={{ color: "var(--fg-subtle)" }}
        >
          {hint}
        </span>
      )}
      <div className="mt-1.5">{children}</div>
    </label>
  );
}

function TemplateEditor({ id }: { id: string }) {
  const [template, setTemplate] = useState<TemplateView | null>(null);
  const [loadError, setLoadError] = useState("");

  // Editable fields.
  const [name, setName] = useState("");
  const [alias, setAlias] = useState("");
  const [subject, setSubject] = useState("");
  const [body, setBody] = useState("");
  const [htmlBody, setHtmlBody] = useState("");

  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState("");
  const [saved, setSaved] = useState(false);

  // Preview state — POST /v1/templates/validate renders the current
  // (possibly unsaved) sources against the test data. suggested_data
  // from the response (nested on the wire) seeds one input per referenced
  // variable, keyed by the dotted variable name; user-entered values win
  // on re-validate. The flat display map is nested back into an object
  // when posted as test_data (see _lib/testdata.ts).
  const [testData, setTestData] = useState<Record<string, string>>({});
  const [validation, setValidation] =
    useState<ValidateTemplateResponse | null>(null);
  const [previewing, setPreviewing] = useState(false);
  const [previewError, setPreviewError] = useState("");

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await fetch(`/v1/templates/${encodeURIComponent(id)}`, {
          credentials: "include",
        });
        if (!res.ok) {
          if (!cancelled)
            setLoadError(`Failed to load template (HTTP ${res.status})`);
          return;
        }
        const t: TemplateView = await res.json();
        if (cancelled) return;
        setTemplate(t);
        setName(t.name);
        setAlias(t.alias ?? "");
        setSubject(t.subject);
        setBody(t.text);
        setHtmlBody(t.html ?? "");
      } catch (err) {
        if (!cancelled)
          setLoadError(err instanceof Error ? err.message : String(err));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [id]);

  const handleSave = async () => {
    setSaving(true);
    setSaveError("");
    setSaved(false);
    try {
      const res = await fetch(`/v1/templates/${encodeURIComponent(id)}`, {
        method: "PATCH",
        credentials: "include",
        headers: { "Content-Type": "application/json" },
        // Send every field: alias "" clears the alias, html ""
        // removes the HTML part — which matches empty inputs.
        body: JSON.stringify({
          name,
          alias,
          subject,
          text: body,
          html: htmlBody,
        }),
      });
      if (!res.ok) {
        const { code, message } = await readErrorBody(res);
        setSaveError(
          code === "alias_taken"
            ? `The alias "${alias}" is already taken by another of your templates.`
            : message,
        );
        return;
      }
      const t: TemplateView = await res.json();
      setTemplate(t);
      setSaved(true);
      setTimeout(() => setSaved(false), 2000);
    } catch (err) {
      setSaveError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const runPreview = useCallback(
    async (data: Record<string, string>) => {
      setPreviewing(true);
      setPreviewError("");
      try {
        const res = await fetch("/v1/templates/validate", {
          method: "POST",
          credentials: "include",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            subject,
            text: body,
            ...(htmlBody ? { html: htmlBody } : {}),
            // Dotted display keys → the nested object the renderer resolves.
            test_data: nestTestData(data),
          }),
        });
        if (!res.ok) {
          const { message } = await readErrorBody(res);
          setPreviewError(message);
          return;
        }
        const v: ValidateTemplateResponse = await res.json();
        setValidation(v);
        // Seed an input for every variable the source references (nested
        // suggested_data → dotted display keys); keep whatever the user
        // already typed.
        if (v.suggested_data) {
          const flat = flattenSuggested(v.suggested_data);
          setTestData((prev) => ({ ...flat, ...prev }));
        }
      } catch (err) {
        setPreviewError(err instanceof Error ? err.message : String(err));
      } finally {
        setPreviewing(false);
      }
    },
    [subject, body, htmlBody],
  );

  if (loadError) {
    return (
      <p className="text-[13px]" style={{ color: "var(--danger-strong)" }}>
        {loadError}
      </p>
    );
  }
  if (!template) {
    return (
      <p className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
        Loading template…
      </p>
    );
  }

  const varNames = Object.keys(testData).sort();

  return (
    <div className="space-y-8" style={{ maxWidth: 760 }}>
      <section className="space-y-4">
        <LabeledField label="Name">
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            className="w-full px-3 py-2 text-[13px]"
            style={fieldStyle}
          />
        </LabeledField>
        <LabeledField
          label="Alias"
          hint="Per-account handle used as template_alias on send. Leave empty to clear."
        >
          <input
            value={alias}
            onChange={(e) => setAlias(e.target.value)}
            placeholder="my-template"
            className="w-full px-3 py-2 text-[13px] font-mono"
            style={fieldStyle}
          />
        </LabeledField>
        <LabeledField label="Subject">
          <input
            value={subject}
            onChange={(e) => setSubject(e.target.value)}
            className="w-full px-3 py-2 text-[13px] font-mono"
            style={fieldStyle}
          />
        </LabeledField>
        <LabeledField
          label="Plain-text body"
          hint="{{variable}} interpolates; every email should carry a text part."
        >
          <textarea
            value={body}
            onChange={(e) => setBody(e.target.value)}
            rows={10}
            className="w-full px-3 py-2 text-[12px] font-mono"
            style={fieldStyle}
          />
        </LabeledField>
        <LabeledField
          label="HTML body"
          hint="Optional. {{variable}} is HTML-escaped; {{{variable}}} inserts raw HTML — never feed it untrusted input. Empty removes the HTML part."
        >
          <textarea
            value={htmlBody}
            onChange={(e) => setHtmlBody(e.target.value)}
            rows={14}
            className="w-full px-3 py-2 text-[12px] font-mono"
            style={fieldStyle}
          />
        </LabeledField>

        <div className="flex items-center gap-3 flex-wrap">
          <Button
            onClick={() => void handleSave()}
            disabled={saving || !name.trim() || !subject.trim() || !body.trim()}
          >
            {saving ? "Saving…" : "Save changes"}
          </Button>
          <Button
            variant="ghost"
            onClick={() => void runPreview(testData)}
            disabled={previewing}
          >
            {previewing ? "Rendering…" : validation ? "Refresh preview" : "Preview"}
          </Button>
          {saved && (
            <span className="text-[12px]" style={{ color: "var(--success)" }}>
              Saved
            </span>
          )}
          {saveError && (
            <span
              className="text-[12px]"
              style={{ color: "var(--danger-strong)" }}
            >
              {saveError}
            </span>
          )}
        </div>
      </section>

      {(validation || previewError) && (
        <section
          aria-label="Preview"
          className="p-5"
          style={{
            background: "var(--bg-panel)",
            border: "1px solid var(--border)",
            borderRadius: "var(--r-lg)",
          }}
        >
          <div className="flex items-center gap-2 mb-3">
            <h2
              className="text-[15px] font-semibold"
              style={{ color: "var(--fg)", margin: 0 }}
            >
              Preview
            </h2>
            <span className="text-[11px]" style={{ color: "var(--fg-subtle)" }}>
              renders the fields above, including unsaved edits
            </span>
          </div>

          {previewError && (
            <p
              className="text-[13px] mb-3"
              style={{ color: "var(--danger-strong)" }}
            >
              {previewError}
            </p>
          )}

          {validation && !validation.valid && (
            <div
              className="mb-4 p-3 text-[13px]"
              style={{
                background: "var(--danger-bg)",
                border: "1px solid var(--danger-bg)",
                color: "var(--danger-strong)",
                borderRadius: "var(--r-md)",
              }}
            >
              <p className="font-medium mb-1">Template does not parse:</p>
              <ul className="m-0 pl-4">
                {validation.errors.map((e, i) => (
                  <li key={i}>
                    <code className="font-mono text-[12px]">{e.part}</code>:{" "}
                    {e.message}
                  </li>
                ))}
              </ul>
            </div>
          )}

          {varNames.length > 0 && (
            <div className="mb-4">
              <div
                className="font-mono text-[10px] uppercase mb-1.5"
                style={{ color: "var(--fg-subtle)", letterSpacing: "0.08em" }}
              >
                Test data
              </div>
              <div className="grid gap-2 sm:grid-cols-2">
                {varNames.map((k) => (
                  <label key={k} className="flex items-center gap-2 min-w-0">
                    <code
                      className="font-mono text-[11px] shrink-0"
                      style={{ color: "var(--fg)", minWidth: 120 }}
                    >
                      {k}
                    </code>
                    <input
                      value={testData[k]}
                      aria-label={`Test value for ${k}`}
                      onChange={(e) =>
                        setTestData((prev) => ({
                          ...prev,
                          [k]: e.target.value,
                        }))
                      }
                      className="flex-1 min-w-0 px-2 py-1 text-[12px]"
                      style={fieldStyle}
                    />
                  </label>
                ))}
              </div>
            </div>
          )}

          {validation?.rendered && (
            <TemplatePreview
              subject={validation.rendered.subject}
              textBody={validation.rendered.text}
              htmlBody={validation.rendered.html}
            />
          )}
        </section>
      )}
    </div>
  );
}

function TemplateEditPageInner() {
  const params = useSearchParams();
  const id = params.get("id") ?? "";

  return (
    <PageShell
      eyebrow="Workspace"
      title={
        <span className="inline-flex items-center gap-2.5">
          Edit template <Chip tone="warn">Beta</Chip>
        </span>
      }
    >
      {id ? (
        // Remount on id change so field state never leaks across templates.
        <TemplateEditor key={id} id={id} />
      ) : (
        <p className="text-[13px]" style={{ color: "var(--danger-strong)" }}>
          Missing ?id= query parameter
        </p>
      )}
    </PageShell>
  );
}

export default function TemplateEditPage() {
  // useSearchParams() must live under a Suspense boundary in Next 16.
  return (
    <Suspense fallback={null}>
      <TemplateEditPageInner />
    </Suspense>
  );
}
