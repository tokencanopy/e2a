"use client";

import {
  LEGACY_SIGN_IN_URL,
  SIGN_IN_LABEL,
  SIGN_IN_URL,
  signInURLWithReturnTo,
} from "../../lib/site";
import { SignInLink } from "./SignInLink";

export function SignInLinks({
  returnTo,
  preserveCurrentPaths,
  primaryClassName,
  primaryStyle,
  secondaryClassName,
  secondaryStyle,
}: {
  returnTo?: string;
  preserveCurrentPaths?: readonly string[];
  primaryClassName?: string;
  primaryStyle?: React.CSSProperties;
  secondaryClassName?: string;
  secondaryStyle?: React.CSSProperties;
}) {
  const hrefFor = (door: string) =>
    returnTo === undefined
      ? door
      : signInURLWithReturnTo(returnTo, door);

  return (
    <>
      <SignInLink
        href={hrefFor(SIGN_IN_URL)}
        preserveCurrentPaths={preserveCurrentPaths}
        className={primaryClassName}
        style={primaryStyle}
      >
        {SIGN_IN_LABEL}
      </SignInLink>
      {SIGN_IN_URL !== LEGACY_SIGN_IN_URL && (
        <SignInLink
          href={hrefFor(LEGACY_SIGN_IN_URL)}
          preserveCurrentPaths={preserveCurrentPaths}
          className={secondaryClassName}
          style={secondaryStyle}
        >
          Sign in with Google
        </SignInLink>
      )}
    </>
  );
}
