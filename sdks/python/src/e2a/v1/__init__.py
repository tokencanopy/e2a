"""Public surface of the e2a v1 SDK.

The canonical request/response types are the OpenAPI-Generator ``generated``
models; the hand-written ergonomic layer (:class:`AsyncE2AClient` + resources,
the typed error hierarchy, retry/pagination, webhook verification, WS) wraps
them. The legacy flat/sync ``api`` / ``client`` / ``handler`` surface and the
old swag-generated Pydantic types have been retired in favour of this.

Naming follows the httpx/openai/anthropic convention: the plain name
:class:`E2AClient` is the synchronous client, ``Async*`` is the async one.
The sync client is a facade over the async client (one implementation of
resources/retry/errors/pagination, bridged through a background event loop) —
see :mod:`e2a.v1.sync_client`.
"""

# Generated request/response models (the canonical types).
from e2a.v1.generated import models  # noqa: F401
from e2a.v1.generated.models import *  # noqa: F401,F403

# High-level async client.
from e2a.v1.client import AsyncE2AClient, async_signup_agent  # noqa: F401

# High-level inbound email facade.
from e2a.v1.inbound import (  # noqa: F401
    AsyncInboundAttachment,
    AsyncInboundEmail,
    AsyncInboundResource,
    InboundAttachment,
    InboundEmail,
    InboundResource,
)

# Synchronous facade over the async client.
from e2a.v1.sync_client import E2AClient, SyncAutoPager, SyncStream, signup_agent  # noqa: F401

# Typed error hierarchy.
from e2a.v1.errors import (  # noqa: F401
    E2AAuthError,
    E2AConflictError,
    E2AConnectionError,
    E2AConnectionReplacedError,
    E2AError,
    E2AIdempotencyError,
    E2ALimitExceededError,
    E2ANotFoundError,
    E2APermissionError,
    E2ARateLimitError,
    E2AServerError,
    E2AValidationError,
    E2AWebhookSignatureError,
)

# Auto-pagination.
from e2a.v1.pagination import AutoPager, Page  # noqa: F401

# Webhook signature verification + the typed per-event payloads and their
# narrowing guards. NOTE: these *Data types are hand-written WIRE-shape
# TypedDicts from webhook_signature, matched to the server's canonical structs
# and the shared golden fixtures. Imported AFTER the generated-models star
# import above, so they deliberately shadow the same-named codegen pydantic
# models in this namespace (those remain reachable as ``models.*Data``).
from e2a.v1.webhook_signature import (  # noqa: F401
    Authentication,
    AttachmentMetaView,
    DKIMResult,
    DMARCResult,
    DomainSendingFailedData,
    DomainSendingVerifiedData,
    DomainSuppressionAddedData,
    EmailBouncedData,
    EmailComplainedData,
    EmailDeliveredData,
    EmailFailedData,
    EmailReceivedData,
    EmailSentData,
    SPFResult,
    WebhookEvent,
    construct_event,
    is_domain_sending_failed,
    is_domain_sending_verified,
    is_domain_suppression_added,
    is_email_bounced,
    is_email_complained,
    is_email_delivered,
    is_email_failed,
    is_email_received,
    is_email_sent,
    verify_webhook_signature,
)

# Real-time WebSocket stream (frames are the same event envelope as webhooks).
from e2a.v1.websocket import WS_CLOSE_REPLACED, WSEvent, WSStream  # noqa: F401

__all__ = [
    "AsyncE2AClient",
    "async_signup_agent",
    "signup_agent",
    "AsyncInboundResource",
    "AsyncInboundEmail",
    "AsyncInboundAttachment",
    "InboundEmail",
    "InboundAttachment",
    "InboundResource",
    "E2AClient",
    "SyncAutoPager",
    "SyncStream",
    "MessageLifecycleTransition",
    "PageMessageLifecycleTransition",
    # Errors
    "E2AError",
    "E2AAuthError",
    "E2APermissionError",
    "E2ANotFoundError",
    "E2AConflictError",
    "E2AValidationError",
    "E2AIdempotencyError",
    "E2ALimitExceededError",
    "E2ARateLimitError",
    "E2AServerError",
    "E2AConnectionError",
    "E2AConnectionReplacedError",
    "E2AWebhookSignatureError",
    # Pagination
    "AutoPager",
    "Page",
    # Webhooks
    "verify_webhook_signature",
    "construct_event",
    "WebhookEvent",
    # Typed per-event payloads (stable events).
    "AttachmentMetaView",
    "SPFResult",
    "DKIMResult",
    "DMARCResult",
    "Authentication",
    "EmailReceivedData",
    "EmailSentData",
    "EmailFailedData",
    "EmailDeliveredData",
    "EmailBouncedData",
    "EmailComplainedData",
    "DomainSendingVerifiedData",
    "DomainSendingFailedData",
    "DomainSuppressionAddedData",
    # Narrowing guards.
    "is_email_received",
    "is_email_sent",
    "is_email_failed",
    "is_email_delivered",
    "is_email_bounced",
    "is_email_complained",
    "is_domain_sending_verified",
    "is_domain_sending_failed",
    "is_domain_suppression_added",
    # WebSocket
    "WS_CLOSE_REPLACED",
    "WSEvent",
    "WSStream",
    # Generated models namespace
    "models",
]
