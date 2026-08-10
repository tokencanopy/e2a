const LITERAL_IDENTIFIER = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;
const REDACTED_IDENTIFIER = /^\[ENV:[A-Z][A-Z0-9_]*\]$/;

export const EVAL_IDENTIFIER_LIMITS = Object.freeze({ suiteNameBytes: 128, caseIdBytes: 128 });

export function isEvalLiteralIdentifier(value, limit) {
  return typeof value === "string"
    && Number.isSafeInteger(limit)
    && Buffer.byteLength(value, "utf8") <= limit
    && LITERAL_IDENTIFIER.test(value);
}

// Artifact case IDs are either validated literals or an environment-name marker.
// Marker contents never include the resolved environment value.
export function isSafeResultCaseId(value) {
  return isEvalLiteralIdentifier(value, EVAL_IDENTIFIER_LIMITS.caseIdBytes)
    || (typeof value === "string" && REDACTED_IDENTIFIER.test(value));
}
