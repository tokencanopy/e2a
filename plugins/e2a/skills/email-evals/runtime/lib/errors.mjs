const ERROR_CLASSES = new Set([
  "configuration_error",
  "capability_error",
  "transport_error",
  "target_timeout",
  "assertion_failure",
  "grader_error",
]);

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
