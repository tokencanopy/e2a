"use client";

// /account/unavailable?code=<code> — where the server sends a sign-in it
// refused. It shows why, in plain words, and never any account data (there is
// no session to read it with). Unknown or missing codes get a generic message
// so a newer server code never renders a blank page.

import Link from "next/link";
import { Suspense } from "react";
import { useSearchParams } from "next/navigation";
import { SignInLinks } from "../../components/SignInLinks";
import {
  AccountHeading,
  AccountShell,
  AccountText,
  actionClassName,
  primaryActionStyle,
  secondaryActionStyle,
} from "../_components/AccountShell";

type Copy = {
  title: string;
  body: string;
  // Offer the sign-in door again only where retrying can change the outcome.
  offerSignIn: boolean;
};

const UNAVAILABLE_COPY: Record<string, Copy> = {
  registration_refused: {
    title: "This sign-in can't be used right now",
    body:
      "The identity you signed in with belongs to an account that was recently deleted or closed. It can't register a new account or restore the old one.",
    offerSignIn: false,
  },
  purge_in_progress: {
    title: "This account is being erased",
    body: "Permanent deletion of this account has already started, so it can't be restored.",
    offerSignIn: false,
  },
  temporarily_unavailable: {
    title: "Sign-in is temporarily unavailable",
    body: "Signing in didn't finish on our side. Try again in a few minutes.",
    offerSignIn: true,
  },
  account_trashed: {
    title: "This account is in the trash",
    body:
      "It was scheduled for deletion. Sign in again to restore it or erase it now, before the trash window ends.",
    offerSignIn: true,
  },
};

const GENERIC_COPY: Copy = {
  title: "This account isn't available",
  body: "Signing in to this account didn't work. Go back to the home page, or try signing in again later.",
  offerSignIn: false,
};

function unavailableCopy(code: string | null | undefined): Copy {
  if (code && Object.prototype.hasOwnProperty.call(UNAVAILABLE_COPY, code)) {
    return UNAVAILABLE_COPY[code];
  }
  return GENERIC_COPY;
}

export default function AccountUnavailablePage() {
  // useSearchParams needs a Suspense boundary for the static export build.
  return (
    <AccountShell>
      <Suspense fallback={<div style={{ minHeight: 200 }} />}>
        <UnavailableInner />
      </Suspense>
    </AccountShell>
  );
}

function UnavailableInner() {
  const search = useSearchParams();
  const copy = unavailableCopy(search?.get("code"));
  return (
    <>
      <AccountHeading>{copy.title}</AccountHeading>
      <AccountText>{copy.body}</AccountText>
      <div className="mt-8 flex flex-wrap items-center gap-x-5 gap-y-3">
        {copy.offerSignIn && (
          <SignInLinks
            primaryClassName={actionClassName}
            primaryStyle={primaryActionStyle}
            secondaryClassName="inline-flex items-center min-h-[44px] text-[14px] underline underline-offset-2"
            secondaryStyle={{ color: "var(--fg-muted)" }}
          />
        )}
        <Link href="/" className={actionClassName} style={secondaryActionStyle}>
          Back to e2a home
        </Link>
      </div>
    </>
  );
}
