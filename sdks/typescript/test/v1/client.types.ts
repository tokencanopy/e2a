import type { FieldError } from "../../src/v1/generated/models/FieldError.js";
import type { ErrorBody } from "../../src/v1/generated/models/ErrorBody.js";
import type {
  Authentication,
  EmailReceivedData,
  DomainSuppressionAddedData,
  EventMessageLifecycleTransition,
  ListMessagesParams,
  MessageLifecycleTransition,
  MessageSummaryView,
  MessageView,
  PageMessageLifecycleTransition,
  SPFResult,
} from "../../src/v1/index.js";
import {
  MessageLifecycleTransitionDirectionEnum,
  MessageLifecycleTransitionOutcomeEnum,
  MessageLifecycleTransitionReasonCodeEnum,
  MessageLifecycleTransitionStageEnum,
  isEmailReceived,
} from "../../src/v1/index.js";
import type { EmailSentData, WebhookEvent } from "../../src/v1/webhook-signature.js";
import type { WSEvent } from "../../src/v1/ws.js";
import { E2AClient } from "../../src/v1/client.js";
import type { InboundEmail } from "../../src/v1/inbound.js";

const senderFilter: ListMessagesParams = { from_: "alice@example.com" };
void senderFilter;

const summaryThreadIDIsOptional: Pick<MessageSummaryView, "threadId"> = {};
const detailThreadIDIsOptional: Pick<MessageView, "threadId"> = {};
void summaryThreadIDIsOptional;
void detailThreadIDIsOptional;

// The pre-GA breaking rename intentionally removes the old public spelling.
// @ts-expect-error `from` is the wire name; SDK callers use `from_`.
const removedSenderFilter: ListMessagesParams = { from: "alice@example.com" };
void removedSenderFilter;

const validationField: FieldError = { location: "", message: "invalid request" };
void validationField;

// @ts-expect-error location is required by the GA validation contract.
const missingValidationLocation: FieldError = { message: "invalid request" };
void missingValidationLocation;

const futureError: ErrorBody = {
  code: "future_error_code",
  message: "future failure",
  requestId: "req_future",
  details: { future_field: { nested: true } },
};
void futureError;

// @ts-expect-error requestId is required on every /v1 error.
const missingErrorRequestId: ErrorBody = { code: "invalid_request", message: "invalid" };
void missingErrorRequestId;

const eventEnvelope: WebhookEvent = {
  type: "email.received",
  id: "evt_1",
  schema_version: "1",
  created_at: "2026-07-01T10:30:00Z",
  data: {},
};
const wsEnvelope: WSEvent = eventEnvelope;
void wsEnvelope;

const wireAuthentication: Authentication = {
  spf: { status: "pass", domain: "example.com", aligned: true },
  dkim: [],
  dmarc: {
    status: "pass",
    domain: "example.com",
    policy: "reject",
    aligned_by: ["spf"],
  },
};
const receivedAuthentication: EmailReceivedData["authentication"] = wireAuthentication;
void receivedAuthentication;

const wireLifecycleTransition: EventMessageLifecycleTransition = {
  id: "mlt_1",
  message_id: "msg_1",
  direction: MessageLifecycleTransitionDirectionEnum.Inbound,
  recipient: null,
  stage: MessageLifecycleTransitionStageEnum.Accepted,
  outcome: MessageLifecycleTransitionOutcomeEnum.Accepted,
  reason_code: MessageLifecycleTransitionReasonCodeEnum.AcceptanceInboundSmtp,
  retryable: false,
  evidence: { source: "smtp", future: { nested: true } },
  correlation_ids: { email_message_id: "<msg@example.com>", future_id: "future" },
  occurred_at: "2026-07-22T00:00:00Z",
  reconstructed: true,
};

const receivedLifecycle: EmailReceivedData["lifecycle_transitions"] = [wireLifecycleTransition];
const suppressionLifecycle: DomainSuppressionAddedData["lifecycle_transitions"] = [wireLifecycleTransition];
void receivedLifecycle;
void suppressionLifecycle;

if (isEmailReceived(wsEnvelope)) {
  const wsLifecycle: EventMessageLifecycleTransition[] | undefined =
    wsEnvelope.data.lifecycle_transitions;
  void wsLifecycle;
}

// @ts-expect-error event lifecycle direction derives the closed generated enum.
const unknownWireDirection: EventMessageLifecycleTransition["direction"] = "sideways";
// @ts-expect-error event lifecycle stage derives the closed generated enum.
const unknownWireStage: EventMessageLifecycleTransition["stage"] = "future_stage";
// @ts-expect-error event lifecycle outcome derives the closed generated enum.
const unknownWireOutcome: EventMessageLifecycleTransition["outcome"] = "future_outcome";
// @ts-expect-error event lifecycle reason derives the closed generated enum.
const unknownWireReason: EventMessageLifecycleTransition["reason_code"] = "provider.free_form";
void unknownWireDirection;
void unknownWireStage;
void unknownWireOutcome;
void unknownWireReason;

// @ts-expect-error RFC 7208 has no SPF "policy" result.
const retiredSPFPolicy: SPFResult["status"] = "policy";
void retiredSPFPolicy;

const loopbackSentData: EmailSentData = {
  message_id: "msg_local",
  agent_email: "bot@example.com",
  direction: "outbound",
  method: "loopback",
  from: "bot@example.com",
  to: ["bot@example.com"],
  subject: "Note to self",
  message_type: "send",
};
void loopbackSentData;

const managedClient = new E2AClient({ apiKey: "e2a_test" });
managedClient.messages.send("sender@example.com", {
  to: ["recipient@example.net"],
  subject: "Update",
  text: "Hello",
  unsubscribe: { mode: "managed" },
});
managedClient.agents.listSuppressions("sender@example.com");
managedClient.agents.createSuppression("sender@example.com", {
  address: "recipient@example.net",
  reason: "recipient opted out",
});
managedClient.agents.deleteSuppression("sender@example.com", "recipient@example.net");

const lifecyclePage: Promise<PageMessageLifecycleTransition> =
  managedClient.messages.getLifecycle("sender@example.com", "msg_1", {
    cursor: "cur_1",
    limit: 25,
  });
void lifecyclePage;

const lifecycleTransition: MessageLifecycleTransition = {
  correlationIds: { request_id: "req_1", future_id: "future_1" },
  direction: MessageLifecycleTransitionDirectionEnum.Outbound,
  evidence: { source: "api", nested: { future: true } },
  id: "mlt_1",
  messageId: "msg_1",
  occurredAt: new Date("2026-07-22T00:00:00Z"),
  outcome: MessageLifecycleTransitionOutcomeEnum.Accepted,
  reasonCode: MessageLifecycleTransitionReasonCodeEnum.AcceptanceOutboundApi,
  recipient: null,
  reconstructed: false,
  retryable: false,
  stage: MessageLifecycleTransitionStageEnum.Accepted,
};
void lifecycleTransition;

// @ts-expect-error lifecycle direction is a closed, versioned vocabulary.
const unknownLifecycleDirection: MessageLifecycleTransition["direction"] = "sideways";
// @ts-expect-error lifecycle stage is a closed, versioned vocabulary.
const unknownLifecycleStage: MessageLifecycleTransition["stage"] = "future_stage";
// @ts-expect-error lifecycle outcome is a closed, versioned vocabulary.
const unknownLifecycleOutcome: MessageLifecycleTransition["outcome"] = "future_outcome";
// @ts-expect-error lifecycle reason code is a closed, versioned vocabulary.
const unknownLifecycleReason: MessageLifecycleTransition["reasonCode"] = "provider.free_form";
void unknownLifecycleDirection;
void unknownLifecycleStage;
void unknownLifecycleOutcome;
void unknownLifecycleReason;

// @ts-expect-error all five core envelope fields are required.
const incompleteEventEnvelope: WebhookEvent = { type: "email.received", data: {} };
void incompleteEventEnvelope;

const inboundEmailPromise: Promise<InboundEmail> = managedClient.inbound.fromEvent(eventEnvelope);
inboundEmailPromise.then((email) => {
  const envelopeFrom: string | null = email.envelopeFrom;
  const verified: boolean = email.verified;
  const targets: readonly string[] = email.replyTargets;
  const result = email.reply({ text: "ok" });
  const attachment = email.attachments[0].get({ inline: true });
  void envelopeFrom;
  void verified;
  void targets;
  void result;
  void attachment;
});
