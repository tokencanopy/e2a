import { normalizePolicy, validatePolicy } from "./policy.mjs";

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
const DOMAIN_RE = /^(?=.{1,253}$)(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;

const always = () => true;
const support = (answers) => answers.profile === "customer-support";
const custom = (answers) => answers.profile === "custom";
const reviewDisabled = (answers) => answers.outbound_review === false;
const ownerCcDisabled = (answers) => answers.owner_cc === false;
const screeningDisabled = (answers) => answers.screening === false;
const customSandbox = (answers) => answers.sandbox === "custom";

const QUESTIONS = [
  {
    id: "objective",
    kind: "text",
    required: true,
    prompt:
      "What task should this always-on email agent solve, and what outcome should it produce?",
  },
  {
    id: "profile",
    kind: "choice",
    choices: ["customer-support", "custom"],
    default: "customer-support",
    prompt:
      "Which task profile should we use? customer-support is the guided starting point; custom grants no extra capabilities. [customer-support]",
  },
  {
    id: "support_scope",
    kind: "text",
    required: true,
    when: support,
    prompt: "Which customer questions may the support agent handle?",
  },
  {
    id: "support_exclusions",
    kind: "text",
    required: true,
    when: support,
    prompt:
      "Which requests must it never handle autonomously (for example refunds, legal, billing, or security)?",
  },
  {
    id: "knowledge_sources",
    kind: "text",
    required: true,
    when: support,
    prompt: "Which approved knowledge sources may it use?",
  },
  {
    id: "escalation_rules",
    kind: "text",
    required: true,
    when: support,
    prompt: "When must it escalate to a human instead of replying?",
  },
  {
    id: "response_expectations",
    kind: "text",
    required: true,
    when: support,
    prompt:
      "What response-time or service-level expectation should guide the support agent?",
  },
  {
    id: "reply_mode",
    kind: "choice",
    choices: ["submit-for-review", "draft-only"],
    default: "submit-for-review",
    when: support,
    prompt:
      "Should the agent submit in-thread replies to e2a review, or only escalate a proposed draft? submit-for-review or draft-only [submit-for-review]",
  },
  {
    id: "tone",
    kind: "text",
    default: "Clear, warm, and concise.",
    when: support,
    prompt: "What tone should replies use? [Clear, warm, and concise.]",
  },
  {
    id: "signature",
    kind: "text",
    required: true,
    when: support,
    prompt: "What signature should the agent use?",
  },
  {
    id: "custom_instructions",
    kind: "text",
    required: true,
    when: custom,
    prompt:
      "Describe the allowed task behavior and explicit escalation boundaries. These instructions do not grant mailbox, filesystem, or network access.",
  },
  {
    id: "agent_email",
    kind: "email",
    prompt: "Which existing e2a agent mailbox should Autopilot listen to?",
  },
  {
    id: "owner_email",
    kind: "email",
    prompt: "Which human owner should receive notifications and be CC'd by default?",
  },
  {
    id: "authorization_mode",
    kind: "choice",
    choices: ["addresses", "domains"],
    default: "addresses",
    prompt:
      "Should this inbox trust exact sender addresses or entire verified domains? The existing e2a gate supports one mode per inbox. [addresses]",
  },
  {
    id: "authorized_senders",
    kind: "senders",
    prompt: (state) =>
      state.answers.authorization_mode === "domains"
        ? "Which verified domains may trigger the agent? Enter comma-separated domains. Everyone else will go to e2a review."
        : "Which exact email addresses may trigger the agent? Enter comma-separated addresses. Everyone else will go to e2a review.",
  },
  {
    id: "outbound_review",
    kind: "boolean",
    default: true,
    prompt: "Require human review for every outbound email? [yes]",
  },
  {
    id: "outbound_review_ack",
    kind: "ack",
    when: reviewDisabled,
    prompt:
      "Warning: without outbound review, the agent may send email without human approval. Type I understand to continue.",
  },
  {
    id: "owner_cc",
    kind: "boolean",
    default: true,
    prompt: "CC the owner on every outbound thread? [yes]",
  },
  {
    id: "owner_cc_ack",
    kind: "ack",
    when: ownerCcDisabled,
    prompt:
      "Warning: without owner CC, outbound threads may not be visible in the owner's mailbox. Type I understand to continue.",
  },
  {
    id: "screening",
    kind: "boolean",
    default: true,
    prompt: "Enable e2a prompt-injection screening? [yes]",
  },
  {
    id: "screening_ack",
    kind: "ack",
    when: screeningDisabled,
    prompt:
      "Warning: disabling screening removes a defense against malicious instructions in authorized email. Type I understand to continue.",
  },
  {
    id: "runtime",
    kind: "choice",
    choices: ["claude", "codex", "hermes", "custom"],
    default: "claude",
    prompt:
      "Which runtime should handle each job? claude, codex, hermes, or custom [claude] (OpenClaw is unavailable in this release: its invocation flags are unverified)",
  },
  {
    id: "runtime_command",
    kind: "absolute-path",
    prompt: "What is the absolute path to that runtime executable?",
  },
  {
    id: "workdir",
    kind: "absolute-path",
    prompt: "What absolute workspace path may the runtime use?",
  },
  {
    id: "sandbox",
    kind: "choice",
    choices: ["custom"],
    default: "custom",
    prompt:
      "There is no isolation profile Autopilot can verify: native coding-agent flags do not stop a same-user process from reading owner-readable files. Configure a container, VM, separate OS user, or equivalent wrapper, then choose custom. [custom]",
  },
  {
    id: "custom_sandbox_ack",
    kind: "ack",
    when: customSandbox,
    prompt:
      "Warning: Autopilot cannot verify a custom isolation boundary. Type I understand to accept responsibility for it.",
  },
  {
    id: "service",
    kind: "choice",
    choices: ["launchd", "systemd", "foreground"],
    default: (state) =>
      state.platform === "darwin"
        ? "launchd"
        : state.platform === "linux"
          ? "systemd"
          : "foreground",
    prompt:
      "How should the supervisor run? launchd, systemd, or foreground/manual [platform default]",
  },
];

export function createInterview({ platform = process.platform } = {}) {
  return {
    version: 1,
    platform,
    answers: {},
  };
}

function applies(question, answers) {
  return (question.when || always)(answers);
}

function publicQuestion(question, state) {
  const defaultValue =
    typeof question.default === "function"
      ? question.default(state)
      : question.default;
  const choices =
    typeof question.choices === "function"
      ? question.choices(state)
      : question.choices;
  return {
    id: question.id,
    prompt:
      typeof question.prompt === "function"
        ? question.prompt(state)
        : question.prompt,
    kind: question.kind,
    ...(choices ? { choices: [...choices] } : {}),
    ...(defaultValue !== undefined ? { default: defaultValue } : {}),
  };
}

export function nextQuestion(state) {
  if (!state || state.version !== 1 || !state.answers) {
    throw new Error("Invalid or unsupported Autopilot interview state.");
  }
  for (const question of QUESTIONS) {
    if (!applies(question, state.answers)) continue;
    if (!Object.hasOwn(state.answers, question.id)) {
      return publicQuestion(question, state);
    }
  }
  return null;
}

function parseBoolean(value, defaultValue) {
  const normalized = String(value ?? "").trim().toLowerCase();
  if (!normalized && defaultValue !== undefined) return defaultValue;
  if (["y", "yes", "true"].includes(normalized)) return true;
  if (["n", "no", "false"].includes(normalized)) return false;
  throw new Error("Answer yes or no.");
}

function parseSenders(value, mode) {
  const entries = String(value ?? "")
    .split(",")
    .map((entry) => entry.trim().toLowerCase())
    .filter(Boolean);
  if (entries.length === 0) {
    throw new Error(
      "Enter at least one authorized email address or domain; public-any-sender mode is not supported.",
    );
  }
  const addresses = [];
  const domains = [];
  for (const entry of entries) {
    if (entry.includes("@")) {
      if (mode !== "addresses") {
        throw new Error("Domain authorization mode accepts domains only.");
      }
      if (!EMAIL_RE.test(entry)) {
        throw new Error(`${entry} is not a valid email address or domain.`);
      }
      addresses.push(entry);
    } else {
      if (mode !== "domains") {
        throw new Error("Address authorization mode accepts exact email addresses only.");
      }
      if (!DOMAIN_RE.test(entry)) {
        throw new Error(`${entry} is not a valid email address or domain.`);
      }
      domains.push(entry);
    }
  }
  return {
    addresses: [...new Set(addresses)].sort(),
    domains: [...new Set(domains)].sort(),
  };
}

function parseAnswer(question, value, state) {
  const raw = String(value ?? "").trim();
  const defaultValue =
    typeof question.default === "function"
      ? question.default(state)
      : question.default;
  const choices =
    typeof question.choices === "function"
      ? question.choices(state)
      : question.choices;

  switch (question.kind) {
    case "text": {
      const result = raw || defaultValue || "";
      if (question.required && !result) throw new Error("This answer is required.");
      return result;
    }
    case "email": {
      const result = raw.toLowerCase();
      if (!EMAIL_RE.test(result)) throw new Error("Enter a valid email address.");
      return result;
    }
    case "choice": {
      const result = (raw || defaultValue || "").toLowerCase();
      if (!choices.includes(result)) {
        throw new Error(`Choose one of: ${choices.join(", ")}.`);
      }
      return result;
    }
    case "boolean":
      return parseBoolean(raw, defaultValue);
    case "senders":
      return parseSenders(raw, state.answers.authorization_mode);
    case "absolute-path":
      if (!raw.startsWith("/")) throw new Error("Enter an absolute path.");
      return raw;
    case "ack":
      if (raw.toLowerCase() !== "i understand") {
        throw new Error("Type I understand to continue.");
      }
      return true;
    default:
      throw new Error(`Unsupported interview question type: ${question.kind}.`);
  }
}

export function answerQuestion(state, value) {
  const current = nextQuestion(state);
  if (!current) throw new Error("The Autopilot interview is already complete.");
  const definition = QUESTIONS.find((question) => question.id === current.id);
  const answer = parseAnswer(definition, value, state);
  return {
    ...state,
    answers: {
      ...state.answers,
      [definition.id]: answer,
    },
  };
}

function supportInstructions(answers) {
  return [
    `Allowed support scope: ${answers.support_scope}`,
    `Never handle: ${answers.support_exclusions}`,
    `Approved knowledge sources: ${answers.knowledge_sources}`,
    `Escalate when: ${answers.escalation_rules}`,
    `Response expectation: ${answers.response_expectations}`,
    answers.reply_mode === "draft-only"
      ? "Reply mode: draft-only. Do not submit a reply; escalate the proposed draft to the owner."
      : "Reply mode: submit-for-review. Submit an in-thread reply; e2a human review remains authoritative.",
    `Reply tone: ${answers.tone}`,
    `Signature: ${answers.signature}`,
  ].join("\n");
}

export function buildPolicyFromInterview(state) {
  const pending = nextQuestion(state);
  if (pending) {
    throw new Error(`Interview is incomplete; next question is ${pending.id}.`);
  }
  const answers = state.answers;
  const acknowledgements = [];
  if (answers.outbound_review_ack) acknowledgements.push("outbound_review_opt_out");
  if (answers.owner_cc_ack) acknowledgements.push("owner_cc_opt_out");
  if (answers.screening_ack) acknowledgements.push("screening_opt_out");
  if (answers.custom_sandbox_ack) {
    acknowledgements.push("custom_sandbox_acknowledged");
  }

  const policy = normalizePolicy({
    task: {
      profile: answers.profile,
      objective: answers.objective,
      replyMode:
        answers.profile === "customer-support"
          ? answers.reply_mode
          : "submit-for-review",
      instructions:
        answers.profile === "customer-support"
          ? supportInstructions(answers)
          : answers.custom_instructions,
    },
    mailbox: {
      agentEmail: answers.agent_email,
      ownerEmail: answers.owner_email,
    },
    inbound: {
      mode: answers.authorization_mode,
      addresses: answers.authorized_senders.addresses,
      domains: answers.authorized_senders.domains,
      fallback: "review",
    },
    outbound: {
      requireReview: answers.outbound_review,
      ccOwner: answers.owner_cc,
    },
    screening: {
      promptInjection: answers.screening,
    },
    runtime: {
      adapter: answers.runtime,
      command: answers.runtime_command,
      workdir: answers.workdir,
      sandbox: answers.sandbox,
    },
    service: {
      manager: answers.service,
    },
    acknowledgements,
  });

  const errors = validatePolicy(policy);
  if (errors.length > 0) {
    throw new Error(`Interview produced an invalid policy:\n- ${errors.join("\n- ")}`);
  }
  return policy;
}
