"use client";

// Explains the external sending access restriction (beta) and hosts its two
// recovery routes: verifying a sending domain (handled on /domains) and
// filing a request for operator review (handled here). Reachable from the
// dashboard notice, the onboarding success panel, the review-queue
// composer's warning, and a failed send's error message.

import { useState } from "react";
import Link from "next/link";
import useSWR from "swr";
import { PageShell } from "../../components/loft/PageShell";
import { useSendingAccess } from "../../components/hooks/useSendingAccess";
import {
  createSendingAccessRequest,
  getSendingAccessRequest,
  type SendingAccessRequest,
} from "../../components/onboarding/api";
import { sendingAccessRequestKey } from "../../../lib/swrKeys";
import {
  isSendingRestricted,
  parseErrorEnvelope,
  sendingAccessEligibilityLabel,
  sendingAccessNoticeCopy,
} from "../../../lib/sendingAccess";

const BILLING_API = (process.env.NEXT_PUBLIC_BILLING_API ?? "").replace(/\/$/, "");

const cardStyle = (borderColor: string, background: string): React.CSSProperties => ({
  borderColor,
  background,
  borderWidth: 1,
  borderStyle: "solid",
  borderRadius: "var(--r-lg)",
});

const inputStyle: React.CSSProperties = {
  background: "var(--bg-panel)",
  border: "1px solid var(--border)",
  borderRadius: "var(--r-md)",
  color: "var(--fg)",
};

const fieldLabel = "block text-[13px] font-medium mb-1.5";

// Open set: the state string is only ever "pending" | "approved" |
// "declined" today, but the server documents it as open — an unrecognized
// future value falls through to a neutral status line rather than
// rendering nothing.
function RequestStatusCard({ request }: { request: SendingAccessRequest }) {
  if (request.state === "pending") {
    return (
      <div role="status" className="p-4 text-[13px]" style={cardStyle("var(--border)", "var(--bg-elev)")}>
        <p className="font-semibold" style={{ color: "var(--fg)" }}>
          Your request is under review
        </p>
        <p className="mt-1" style={{ color: "var(--fg-muted)" }}>
          We&apos;ll follow up by email once an operator decides.
        </p>
      </div>
    );
  }
  if (request.state === "approved") {
    return (
      <div role="status" className="p-4 text-[13px]" style={cardStyle("var(--accent)", "var(--bg-elev)")}>
        <p className="font-semibold" style={{ color: "var(--fg)" }}>
          Request approved
        </p>
        <p className="mt-1" style={{ color: "var(--fg-muted)" }}>
          An operator approved external sending for this account through the
          shared sending identity.
        </p>
      </div>
    );
  }
  if (request.state === "declined") {
    return (
      <div role="status" className="p-4 text-[13px]" style={cardStyle("var(--border)", "var(--bg-elev)")}>
        <p className="font-semibold" style={{ color: "var(--fg)" }}>
          Request declined
        </p>
        <p className="mt-1" style={{ color: "var(--fg-muted)" }}>
          <Link href="/feedback" className="underline" style={{ color: "var(--accent-strong)" }}>
            Contact support to appeal
          </Link>
          , or file a new request below.
        </p>
      </div>
    );
  }
  return (
    <div role="status" className="p-4 text-[13px]" style={cardStyle("var(--border)", "var(--bg-elev)")}>
      <p style={{ color: "var(--fg-muted)" }}>Request status: {request.state}</p>
    </div>
  );
}

export default function SendingAccessPage() {
  const { status, isLoading: statusLoading } = useSendingAccess();
  const {
    data: request,
    error: requestError,
    isLoading: requestLoading,
    mutate: mutateRequest,
  } = useSWR<SendingAccessRequest | null>(sendingAccessRequestKey, getSendingAccessRequest);

  const [useCase, setUseCase] = useState("");
  const [recipients, setRecipients] = useState("");
  const [volume, setVolume] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [formError, setFormError] = useState("");
  const [rateLimited, setRateLimited] = useState(false);

  const restricted = isSendingRestricted(status);
  const eligibilityLabel = sendingAccessEligibilityLabel(status);
  const canFileRequest = !request || request.state === "declined";
  const showForm = restricted && canFileRequest;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const dailyVolume = Number.parseInt(volume, 10);
    if (!useCase.trim() || !recipients.trim() || !Number.isFinite(dailyVolume) || dailyVolume < 1) {
      return;
    }
    setSubmitting(true);
    setFormError("");
    setRateLimited(false);
    try {
      const created = await createSendingAccessRequest({
        use_case: useCase.trim(),
        recipients: recipients.trim(),
        expected_daily_volume: dailyVolume,
      });
      await mutateRequest(created, { revalidate: false });
      setUseCase("");
      setRecipients("");
      setVolume("");
    } catch (err) {
      const raw = err instanceof Error ? err.message : "Failed to submit request";
      const parsed = parseErrorEnvelope(raw);
      if (parsed.code === "rate_limited") {
        setRateLimited(true);
      } else {
        setFormError(parsed.message);
      }
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <PageShell
      eyebrow="Account"
      title="External sending access"
      subtitle="e2a's shared sending identity is scoped by default. This explains what's allowed today and how to lift the restriction for this account."
      maxWidth={720}
    >
      <div className="space-y-6">
        {statusLoading ? (
          <p className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
            Loading…
          </p>
        ) : !status?.enforcement_applies ? (
          <div className="p-4 text-[13px]" style={cardStyle("var(--border)", "var(--bg-elev)")}>
            External sending is not restricted for this account.
          </div>
        ) : restricted ? (
          <div className="p-4" style={cardStyle("var(--warn-bg)", "var(--warn-bg)")}>
            <p className="text-[14px] font-semibold" style={{ color: "var(--warn-strong)" }}>
              {sendingAccessNoticeCopy(status, { billingEnabled: Boolean(BILLING_API) }).headline}
            </p>
            <p className="text-[13px] mt-1.5" style={{ color: "var(--fg-muted)" }}>
              {sendingAccessNoticeCopy(status, { billingEnabled: Boolean(BILLING_API) }).body}
            </p>
            <p className="text-[13px] mt-3">
              <Link href="/domains" className="underline font-medium" style={{ color: "var(--warn-strong)" }}>
                Verify a domain
              </Link>
              {BILLING_API && (
                <>
                  {" · "}
                  <Link href="/billing" className="underline font-medium" style={{ color: "var(--warn-strong)" }}>
                    Choose a paid plan
                  </Link>
                </>
              )}
            </p>
          </div>
        ) : (
          <div className="p-4" style={cardStyle("var(--accent)", "var(--bg-elev)")}>
            <p className="text-[14px] font-semibold" style={{ color: "var(--fg)" }}>
              No approval needed
            </p>
            <p className="text-[13px] mt-1.5" style={{ color: "var(--fg-muted)" }}>
              {eligibilityLabel ?? "External sending is enabled for this account."}
            </p>
          </div>
        )}

        {requestLoading ? (
          <p className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
            Loading your request…
          </p>
        ) : requestError ? (
          <p role="alert" className="text-[13px]" style={{ color: "var(--danger-strong)" }}>
            Couldn&apos;t load your request. {requestError.message}
          </p>
        ) : request ? (
          <RequestStatusCard request={request} />
        ) : null}

        {showForm && (
          <form onSubmit={submit} className="space-y-4 p-5" style={cardStyle("var(--border)", "var(--bg-panel)")}>
            <h2 className="text-[15px] font-semibold" style={{ color: "var(--fg)" }}>
              Request approval
            </h2>

            <div>
              <label htmlFor="sending-access-use-case" className={fieldLabel} style={{ color: "var(--fg)" }}>
                What are you building?
              </label>
              <textarea
                id="sending-access-use-case"
                value={useCase}
                onChange={(e) => setUseCase(e.target.value)}
                placeholder="What you're building and why it needs to email external recipients"
                rows={4}
                maxLength={2000}
                required
                className="w-full text-[13px] px-3 py-2 resize-none"
                style={inputStyle}
              />
            </div>

            <div>
              <label htmlFor="sending-access-recipients" className={fieldLabel} style={{ color: "var(--fg)" }}>
                Who will you email?
              </label>
              <input
                id="sending-access-recipients"
                type="text"
                value={recipients}
                onChange={(e) => setRecipients(e.target.value)}
                placeholder="For example: your own customers who signed up, your team"
                maxLength={1000}
                required
                className="w-full text-[13px] px-3 py-2"
                style={inputStyle}
              />
            </div>

            <div>
              <label htmlFor="sending-access-volume" className={fieldLabel} style={{ color: "var(--fg)" }}>
                Expected daily volume
              </label>
              <input
                id="sending-access-volume"
                type="number"
                inputMode="numeric"
                min={1}
                max={1000000}
                value={volume}
                onChange={(e) => setVolume(e.target.value)}
                placeholder="Recipients per day"
                required
                className="w-full text-[13px] px-3 py-2"
                style={inputStyle}
              />
            </div>

            <button
              type="submit"
              disabled={submitting}
              className="text-[13px] font-medium px-4 py-2.5 transition disabled:opacity-50 disabled:cursor-not-allowed"
              style={{
                background: "var(--accent-fill)",
                color: "var(--accent-fg)",
                borderRadius: "var(--r-md)",
              }}
            >
              {submitting ? "Submitting…" : "Submit request"}
            </button>

            {rateLimited && (
              <p className="text-[12px]" style={{ color: "var(--warn-strong)" }}>
                You&apos;ve reached the limit of 3 requests per 30 days. Try again later, or{" "}
                <Link href="/feedback" className="underline">
                  contact support
                </Link>
                .
              </p>
            )}
            {formError && (
              <p role="alert" className="text-[12px]" style={{ color: "var(--danger-strong)" }}>
                {formError}
              </p>
            )}
          </form>
        )}
      </div>
    </PageShell>
  );
}
