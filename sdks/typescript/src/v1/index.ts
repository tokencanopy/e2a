// Public surface of the e2a v1 SDK.
//
// The canonical request/response types are the OpenAPI-Generator `generated/`
// models; the hand-written ergonomic layer (E2AClient + resources, errors,
// retry, pagination, webhook verification, WS) wraps them. The legacy
// hand-written v2 `api.ts` / `inbound-email.ts` surface and the old
// swag-generated types were retired. The current `inbound.ts` facade is an
// additive v1 domain layer over the generated MessageView.

// Generated request/response models (types + the small value classes).
export * from "./generated/models/all.js";

// High-level client and its per-resource parameter types.
export { E2AClient, signupAgent } from "./client.js";
export type {
  E2AClientOptions,
  AgentSignupOptions,
  RequestOptions,
  SendOptions,
  ManagedUnsubscribeOptions,
  SendEmailInput,
  ReplyInput,
  ForwardInput,
  ListMessagesParams,
  ListEventsParams,
} from "./client.js";

// High-level inbound email facade for verified webhook and authenticated
// WebSocket event envelopes.
export { InboundResource, InboundEmail, InboundAttachment } from "./inbound.js";
export type {
  EmailReceivedEvent,
  InboundMessageOperations,
  InboundProjection,
  InboundAttachmentProjection,
} from "./inbound.js";

// Typed error hierarchy.
export {
  E2AError,
  E2AAuthError,
  E2APermissionError,
  E2ANotFoundError,
  E2AConflictError,
  E2AValidationError,
  E2AIdempotencyError,
  E2ALimitExceededError,
  E2ARateLimitError,
  E2AServerError,
  E2AConnectionError,
  E2AConnectionReplacedError,
  E2AWebhookSignatureError,
} from "./errors.js";

// Retry + auto-pagination primitives (exported for advanced configuration).
export { RetryHttpLibrary } from "./retry.js";
export type { RetryOptions } from "./retry.js";
export { AutoPager } from "./pagination.js";
export type { Page, FetchPage, AutoPagerOptions } from "./pagination.js";

// Webhook signature verification + the typed per-event payloads and their
// narrowing guards. NOTE: the *Data payload types are hand-written WIRE-shape
// (snake_case) interfaces from webhook-signature.ts, matched to the server's
// canonical structs and the shared golden fixtures. These EXPLICIT exports
// take precedence over the same-named codegen models in the `export *` above
// (ES star-export conflict resolution) — the generated camelCase models are
// not the webhook wire shape.
export {
  verifyWebhookSignature,
  constructEvent,
  isEmailReceived,
  isEmailSent,
  isEmailFailed,
  isEmailDelivered,
  isEmailBounced,
  isEmailComplained,
  isDomainSendingVerified,
  isDomainSendingFailed,
  isDomainSuppressionAdded,
} from "./webhook-signature.js";
export type {
  VerifySignatureOptions,
  ConstructEventOptions,
  WebhookEvent,
  EventMessageLifecycleTransition,
  AttachmentMetaView,
  SPFResult,
  DKIMResult,
  DMARCResult,
  Authentication,
  EmailReceivedData,
  EmailSentData,
  EmailFailedData,
  EmailDeliveredData,
  EmailBouncedData,
  EmailComplainedData,
  DomainSendingVerifiedData,
  DomainSendingFailedData,
  DomainSuppressionAddedData,
} from "./webhook-signature.js";

// Real-time WebSocket stream. Frames are the SAME versioned event envelope as
// webhook deliveries (WebhookEvent) — WSEvent is an alias.
export { WSListener, WSStream, WS_CLOSE_REPLACED } from "./ws.js";
export type { WSListenerOptions, WSListenerEvents, WSEvent } from "./ws.js";

// Friendly cross-language aliases for the most-used response shapes — mirror
// the names the Python SDK exports so users reach for the same vocabulary.
import type {
  PageMessageSummaryView,
  MessageSummaryView,
  SendResultView,
  DeploymentInfoView,
} from "./generated/index.js";
export type MessageList = PageMessageSummaryView;
export type MessageSummary = MessageSummaryView;
export type SendResult = SendResultView;
export type DeploymentInfo = DeploymentInfoView;
