const ERROR_CLASSES = new Set([
  "configuration_error",
  "capability_error",
  "transport_error",
  "target_timeout",
  "assertion_failure",
  "grader_error",
]);

const ADAPTER_TRANSPORT_CODES = [
  "agent_lookup_failed", "ambiguous_correlation", "baseline_limit_exceeded", "baseline_read_failed",
  "conflicting_event_ref", "conflicting_evidence", "invalid_clock", "invalid_timestamp", "malformed_event",
  "malformed_lifecycle", "malformed_message", "malformed_page", "message_identity_mismatch",
  "mime_observation_failed", "observation_failed", "observation_limit_exceeded", "poll_failed",
  "raw_mime_unavailable", "send_acceptance_unknown", "stimulus_identity_mismatch", "stimulus_not_delivered",
  "stimulus_not_observed", "stimulus_send_failed",
];

const ERROR_CODE_ORIGINS = {
  // Configuration and preflight failures abort the run and are never CaseRecords.
  configuration_error: {},
  capability_error: { required_evidence_unavailable: ["grader"] },
  transport_error: {
    ...Object.fromEntries(ADAPTER_TRANSPORT_CODES.map((code) => [code, ["adapter"]])),
    adapter_failed: ["adapter_boundary"],
    artifact_limit_exceeded: ["runner"],
    cases_artifact_limit: ["runner"],
    invalid_clock_after_send: ["runner"],
    invalid_evidence: ["runner", "grader"],
  },
  target_timeout: { no_terminal_response: ["runner"] },
  assertion_failure: { assertions_failed: ["grader"] },
  grader_error: { grader_threw: ["grader"] },
};

export const EVAL_ERROR_CODE_REGISTRY = Object.freeze(Object.fromEntries(
  Object.entries(ERROR_CODE_ORIGINS).map(([errorClass, codes]) => [errorClass, Object.freeze(Object.keys(codes))]),
));

export const EVAL_ERROR_ORIGIN_REGISTRY = Object.freeze(Object.fromEntries(
  Object.entries(ERROR_CODE_ORIGINS).map(([errorClass, codes]) => [errorClass, Object.freeze(Object.fromEntries(
    Object.entries(codes).map(([code, origins]) => [code, Object.freeze([...origins])]),
  ))]),
));

export function isStableEvalErrorCode(errorClass, code) {
  return typeof code === "string" && EVAL_ERROR_CODE_REGISTRY[errorClass]?.includes(code) === true;
}

export function isStableEvalErrorOrigin(errorClass, code, origin) {
  return typeof origin === "string"
    && EVAL_ERROR_ORIGIN_REGISTRY[errorClass]?.[code]?.includes(origin) === true;
}

export class EvalError extends Error {
  constructor(errorClass, code, message, details) {
    if (!ERROR_CLASSES.has(errorClass)) {
      throw new TypeError(`Unsupported evaluation error class: ${errorClass}`);
    }
    super(message);
    this.name = "EvalError";
    this.errorClass = errorClass;
    this.code = code;
    this.details = details;
  }

  toJSON() {
    const result = {
      class: this.errorClass,
      code: this.code,
      message: this.message,
    };
    if (this.details !== undefined) result.details = this.details;
    return result;
  }
}

export const EVAL_ERROR_CLASSES = Object.freeze([...ERROR_CLASSES]);
