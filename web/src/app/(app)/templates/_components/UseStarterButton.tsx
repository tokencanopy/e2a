"use client";

import { useState } from "react";
import { useRouter } from "next/navigation";
import { Button } from "@e2a/ui";
import { readErrorBody, type TemplateView } from "../_lib/types";

// "Use this template" — POST /v1/templates { from_starter } and navigate
// to the new template's edit view. A 409 alias_taken (the user already
// has a template on the starter's default alias) flips into an inline
// prompt for a different alias and retries with { from_starter, alias }.

export function UseStarterButton({
  starterAlias,
  variant = "primary",
}: {
  starterAlias: string;
  variant?: "primary" | "ghost";
}) {
  const router = useRouter();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [conflict, setConflict] = useState(false);
  const [alias, setAlias] = useState("");

  const create = async (aliasOverride?: string) => {
    setBusy(true);
    setError("");
    try {
      const res = await fetch("/v1/templates", {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          from_starter: starterAlias,
          ...(aliasOverride ? { alias: aliasOverride } : {}),
        }),
      });
      if (!res.ok) {
        const { code, message } = await readErrorBody(res);
        if (code === "alias_taken") {
          setConflict(true);
          if (!aliasOverride) setAlias(`${starterAlias}-2`);
          setError(
            aliasOverride
              ? "That alias is taken too — try another."
              : `You already have a template on the alias "${starterAlias}" — pick a different one.`,
          );
        } else {
          setError(message);
        }
        return;
      }
      const body: TemplateView = await res.json();
      router.push(`/templates/edit?id=${encodeURIComponent(body.id)}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  if (conflict) {
    return (
      <div className="flex flex-col gap-1.5 min-w-0">
        <div className="flex gap-1.5 items-center flex-wrap">
          <input
            autoFocus
            value={alias}
            onChange={(e) => setAlias(e.target.value)}
            aria-label="New template alias"
            placeholder="new-alias"
            className="font-mono text-[12px] px-2.5 py-1.5 min-w-0"
            style={{
              width: 160,
              background: "var(--bg-panel)",
              border: "1px solid var(--border)",
              borderRadius: "var(--r-md)",
              color: "var(--fg)",
            }}
          />
          <Button
            variant="ghost"
            disabled={busy || !alias.trim()}
            onClick={() => void create(alias.trim())}
          >
            {busy ? "Creating…" : "Create as this alias"}
          </Button>
          <Button
            variant="ghost"
            disabled={busy}
            onClick={() => {
              setConflict(false);
              setError("");
            }}
          >
            Cancel
          </Button>
        </div>
        {error && (
          <p className="text-[11px]" style={{ color: "var(--danger-strong)" }}>
            {error}
          </p>
        )}
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-1.5 min-w-0">
      <Button variant={variant} disabled={busy} onClick={() => void create()}>
        {busy ? "Creating…" : "Use this template"}
      </Button>
      {error && (
        <p className="text-[11px]" style={{ color: "var(--danger-strong)" }}>
          {error}
        </p>
      )}
    </div>
  );
}
