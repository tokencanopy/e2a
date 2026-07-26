"""The e2a high-level async client (Slice 8c).

A thin, namespaced ergonomic layer over the generated ``generated/`` base: resource
sub-clients (``client.agents``, ``client.messages``, …) wrap the generated
``*Api`` classes (composition, never inheritance), map the generated
``ApiException`` to the typed :mod:`e2a.v1.errors` hierarchy, and expose cursor
lists as an :class:`~e2a.v1.pagination.AutoPager`. Async-only.
"""

from __future__ import annotations

import os
import warnings
from typing import Any, Awaitable, Callable, List, Literal, Optional, Protocol, Sequence, Type, TypeVar, Union

from pydantic import ValidationError

from ._retry import RetryConfig, request_with_retry
from .errors import E2AError, E2AServerError, E2AValidationError
from .generated.api.account_api import AccountApi
from .generated.api.agents_api import AgentsApi
from .generated.api.conversations_api import ConversationsApi
from .generated.api.domains_api import DomainsApi
from .generated.api.events_api import EventsApi
from .generated.api.messages_api import MessagesApi
from .generated.api.meta_api import MetaApi
from .generated.api.reviews_api import ReviewsApi
from .generated.api.templates_api import TemplatesApi
from .generated.api.webhooks_api import WebhooksApi
from .generated.api_client import ApiClient
from .generated.configuration import Configuration
from .generated.models import (
    AgentView,
    AgentSuppressionView,
    APIKeyView,
    ApproveRequest,
    ConversationDetailView,
    ConversationSummaryView,
    CreateAgentRequest,
    CreateAgentSuppressionRequest,
    CreateAPIKeyRequest,
    CreateAPIKeyResponse,
    CreateWebhookRequest,
    CreateWebhookResponse,
    DeleteAgentResult,
    DeleteApiKeyResult,
    DeleteDomainResult,
    DeleteMessageResult,
    DeleteSuppressionResult,
    DeleteTemplateResult,
    DeleteUserDataResult,
    DeleteWebhookResult,
    DeploymentInfoView,
    DomainView,
    EventView,
    ForwardRequest,
    AccountView,
    AttachmentView,
    MessageSummaryView,
    PageMessageLifecycleTransition,
    MessageView,
    RedeliverEventRequest,
    RedeliverView,
    RegisterDomainRequest,
    RejectRequest,
    RejectResultView,
    ReplyRequest,
    ReviewView,
    RotateSecretResponse,
    SendEmailRequest,
    SendResultView,
    StarterTemplateDetailView,
    StarterTemplateView,
    SuppressionView,
    TemplateSummaryView,
    TemplateView,
    TestWebhookResponse,
    TestWebhookRequest,
    UnsubscribeOptions,
    ProtectionConfigView,
    ProtectionConfigRequest,
    CreateTemplateRequest,
    UpdateAgentRequest,
    UpdateMessageRequest,
    UpdateMessageResultView,
    UpdateTemplateRequest,
    UpdateWebhookRequest,
    ValidateTemplateRequest,
    ValidateTemplateResponse,
    UserExport,
    VerifyDomainView,
    WebhookDeliveryView,
    WebhookView,
)
from .pagination import AutoPager, Page
from .inbound import AsyncInboundResource

__all__ = ["AsyncE2AClient"]

T = TypeVar("T")
_Make = Callable[[Optional["dict[str, str]"]], Awaitable[Any]]
# A request body accepted as the typed model or a plain dict.
Body = Union[Any, dict]
# The managed-unsubscribe opt-in (beta), accepted as the typed model or a plain
# dict — mirrors the TS SDK's ManagedUnsubscribeOptions ({"mode": "managed"}).
UnsubscribeInput = Union[UnsubscribeOptions, dict]


class EventLike(Protocol):
    """Structural type for the versioned event envelope — anything with a
    ``type`` and a ``data`` payload. Both :class:`WebhookEvent` and the
    WebSocket channel's ``WSEvent`` satisfy it (the two channels carry the
    same envelope), so envelope-consuming helpers like
    ``client.webhooks.fetch_message`` accept either without importing the
    optional ``websockets``-backed module.
    """

    @property
    def type(self) -> str: ...

    @property
    def data(self) -> Any: ...

DEFAULT_BASE_URL = "https://api.e2a.dev"


def _env(name: str) -> Optional[str]:
    v = os.environ.get(name)
    return v or None


def _resolve_base_url() -> Optional[str]:
    """Read the API host from the environment.

    Canonical is ``E2A_API_URL`` — the same concept the server names with
    ``E2A_API_URL`` (its externally visible API base). ``E2A_BASE_URL`` is the
    name the SDKs shipped with; still honoured so published integrations keep
    working, with a deprecation warning.
    """
    canonical = _env("E2A_API_URL")
    if canonical:
        return canonical
    legacy = _env("E2A_BASE_URL")
    if legacy:
        warnings.warn(
            "E2A_BASE_URL is deprecated — rename it to E2A_API_URL. "
            "The old name still works for now but will be dropped.",
            DeprecationWarning,
            stacklevel=3,
        )
    return legacy


def _coerce(model_cls: Type[T], body: Optional[Body]) -> T:
    if body is None:
        return model_cls()  # type: ignore[call-arg]
    if isinstance(body, model_cls):
        return body
    # A DIFFERENT generated model — e.g. the View returned by a GET fed back
    # into a write whose body type is the Request twin (protection's
    # read-modify-write flow after the request/response schema split). Convert
    # via its dict form so the natural get -> modify -> replace loop keeps
    # working; pydantic then validates against the Request schema as usual.
    if hasattr(body, "to_dict"):
        body = body.to_dict()  # type: ignore[union-attr]
    try:
        return model_cls.model_validate(body)  # type: ignore[attr-defined]
    except ValidationError as e:
        # A bad request body is the caller's input error — surface it typed
        # rather than leaking a raw pydantic ValidationError.
        raise E2AValidationError(
            code="invalid_request_body",
            message=f"invalid request body for {model_cls.__name__}: {e}",
            status=0,
            retryable=False,
        ) from e


class _TypedApiClient(ApiClient):
    """Map malformed successful responses before the retry boundary sees them."""

    def response_deserialize(self, response_data: Any, response_types_map: Any = None) -> Any:
        try:
            return super().response_deserialize(response_data, response_types_map)
        except ValidationError as e:
            headers = response_data.getheaders()
            request_id = headers.get("x-request-id") if headers else None
            raise E2AServerError(
                code="invalid_response",
                message="e2a API returned a response that does not match the v1 schema",
                status=int(getattr(response_data, "status", 0) or 0),
                request_id=request_id,
                retryable=False,
                details=e.errors(include_url=False),
            ) from e


class AsyncE2AClient:
    """Async client for the e2a /v1 API.

    Use as an async context manager so the underlying HTTP connections are
    closed::

        async with AsyncE2AClient(api_key="e2a_...") as client:
            agents = await client.agents.list().to_list(limit=100)
    """

    def __init__(
        self,
        api_key: Optional[str] = None,
        *,
        base_url: Optional[str] = None,
        max_retries: int = 2,
        max_elapsed_ms: Optional[float] = None,
        timeout_ms: Optional[float] = 30_000.0,
        _retry_config: Optional[RetryConfig] = None,
    ) -> None:
        key = api_key or _env("E2A_API_KEY")
        if not key:
            raise E2AError(
                code="no_api_key",
                message="api_key is required — pass api_key=... or set E2A_API_KEY",
                status=0,
                retryable=False,
            )
        self._api_key = key
        self._base_url = base_url or _resolve_base_url() or DEFAULT_BASE_URL
        self._cfg = _retry_config or RetryConfig(
            max_retries=max_retries, max_elapsed_ms=max_elapsed_ms
        )

        config = Configuration(host=self._base_url, access_token=key)
        self._api_client = _TypedApiClient(config)

        # Per-request timeout (default 30s). The generated httpx transport applies
        # `_request_timeout or 300s` per call; we inject our default when the caller
        # didn't pass one, so every request is bounded without threading a timeout
        # through each resource method. A timeout raises httpx.TimeoutException (a
        # TransportError), which the retry layer treats as a retryable connection
        # failure. Pass timeout_ms=None or 0 to fall back to the transport default.
        self._timeout_s = (timeout_ms / 1000.0) if timeout_ms and timeout_ms > 0 else None
        if self._timeout_s is not None:
            _rest = self._api_client.rest_client
            _orig_request = _rest.request

            # Assumes the generated ApiClient calls rest_client.request(...) with
            # `_request_timeout` as a KEYWORD (it does — see generated
            # api_client.py). If a future openapi-generator bump passes it
            # positionally, `.get()` would miss it and we'd re-inject as a kwarg
            # → TypeError; `make generate-sdk-check` (CI) gates that drift, and a
            # regen would surface it here. Keep this in sync if that call shape
            # changes.
            async def _request_with_default_timeout(*args: Any, **kwargs: Any) -> Any:
                if kwargs.get("_request_timeout") is None:
                    kwargs["_request_timeout"] = self._timeout_s
                return await _orig_request(*args, **kwargs)

            _rest.request = _request_with_default_timeout  # type: ignore[method-assign]

        self.agents = AgentsResource(AgentsApi(self._api_client), self)
        self.messages = MessagesResource(MessagesApi(self._api_client), self)
        self.inbound = AsyncInboundResource(self.messages)
        self.conversations = ConversationsResource(ConversationsApi(self._api_client), self)
        self.domains = DomainsResource(DomainsApi(self._api_client), self)
        self.events = EventsResource(EventsApi(self._api_client), self)
        self.webhooks = WebhooksResource(WebhooksApi(self._api_client), self)
        self.account = AccountResource(AccountApi(self._api_client), self)
        self.reviews = ReviewsResource(ReviewsApi(self._api_client), self)
        self.templates = TemplatesResource(TemplatesApi(self._api_client), self)
        self._meta = MetaApi(self._api_client)

    # ── lifecycle ───────────────────────────────────────────────────
    async def aclose(self) -> None:
        await self._api_client.close()

    async def __aenter__(self) -> "AsyncE2AClient":
        return self

    async def __aexit__(self, *exc: Any) -> None:
        await self.aclose()

    # ── shared executors (retry + error mapping live here) ──────────
    async def _read(self, make: _Make) -> Any:
        return await request_with_retry(make, cfg=self._cfg, retryable=True, idempotency=False)

    async def _write_keyed(self, make: _Make, idempotency_key: Optional[str]) -> Any:
        # send/reply/forward/approve/create-api-key/create-webhook: the server
        # dedupes on the key, so retrying the same serialized request is safe.
        return await request_with_retry(
            make, cfg=self._cfg, retryable=True, idempotency=True, idempotency_key=idempotency_key
        )

    async def _write_idempotent(self, make: _Make) -> Any:
        # PUT/PATCH/DELETE: HTTP-idempotent → safe to retry.
        return await request_with_retry(make, cfg=self._cfg, retryable=True, idempotency=True)

    async def _write_unsafe(self, make: _Make) -> Any:
        # Bare POST (create/reject/redeliver/test): NOT retried (avoid double-create).
        return await request_with_retry(make, cfg=self._cfg, retryable=False, idempotency=False)

    # ── public top-level ────────────────────────────────────────────
    async def info(self) -> DeploymentInfoView:
        """Public deployment metadata."""
        return await self._read(lambda h: self._meta.get_info(_headers=h))

    def listen(self, email: str) -> Any:
        """Open a notification stream for an agent's inbox.

        Yields :class:`~e2a.v1.websocket.WSEvent` envelopes — the same
        versioned shape as webhook deliveries (``email.received`` today;
        tolerate unknown types). Fetch the body when you need it::

            async for event in client.listen("bot@acme.dev"):
                if event.type != "email.received":
                    continue
                msg = await client.messages.get(event.data["delivered_to"], event.data["message_id"])
        """
        if not email:
            raise E2AError(
                code="missing_email",
                message="email is required — pass client.listen(email)",
                status=0,
                retryable=False,
            )
        from .websocket import WSStream  # local import: optional `websockets` dep

        return WSStream(api_key=self._api_key, agent_email=email, base_url=self._base_url)


def _page(items: Optional[Sequence[T]], next_cursor: Optional[str] = None) -> Page:
    return Page(items=items or [], next_cursor=next_cursor)


class AgentsResource:
    """Agent administration. Exact-agent suppression management is beta and
    may change before it is declared stable."""

    def __init__(self, api: AgentsApi, client: AsyncE2AClient) -> None:
        self._api = api
        self._c = client

    def list(
        self, *, limit: Optional[int] = None, deleted: Optional[bool] = None
    ) -> AutoPager[AgentView]:
        # Cursor-paginated: the AutoPager walks next_cursor to completion.
        async def fetch(cursor: Optional[str]) -> Page:
            resp = await self._c._read(
                lambda h: self._api.list_agents(
                    cursor=cursor, limit=limit, deleted=deleted, _headers=h
                )
            )
            return _page(resp.items, resp.next_cursor)

        return AutoPager(fetch)

    async def get(self, email: str) -> AgentView:
        return await self._c._read(lambda h: self._api.get_agent(email, _headers=h))

    async def create(self, body: Body) -> AgentView:
        req = _coerce(CreateAgentRequest, body)
        return await self._c._write_unsafe(lambda h: self._api.create_agent(req, _headers=h))

    async def update(self, email: str, patch: Body) -> AgentView:
        req = _coerce(UpdateAgentRequest, patch)
        return await self._c._write_idempotent(
            lambda h: self._api.update_agent(email, req, _headers=h)
        )

    async def get_protection(self, email: str) -> ProtectionConfigView:
        """Read an agent's protection config (gate + scan sensitivity + holds).

        Beta; account scope only — an agent-scoped key cannot read its own config.
        """
        return await self._c._read(lambda h: self._api.get_agent_protection(email, _headers=h))

    async def replace_protection(self, email: str, config: Body) -> ProtectionConfigView:
        """Replace an agent's protection config wholesale (all three top-level
        keys required). Beta; account scope only. PUT is idempotent."""
        req = _coerce(ProtectionConfigRequest, config)
        return await self._c._write_idempotent(
            lambda h: self._api.put_agent_protection(email, req, _headers=h)
        )

    async def delete(self, email: str) -> DeleteAgentResult:
        # The typed .delete() call is the confirmation; the SDK supplies the
        # ?confirm=DELETE guard the raw API requires (AG-6). Returns the deletion
        # receipt ({deleted, email, messages_deleted}).
        return await self._c._write_idempotent(lambda h: self._api.delete_agent(email, confirm="DELETE", _headers=h))

    async def restore(self, email: str) -> AgentView:
        """Restore an agent from the 30-day trash. Account scope only."""
        return await self._c._write_unsafe(
            lambda h: self._api.restore_agent(email, _headers=h)
        )

    async def test(self, email: str) -> SendResultView:
        return await self._c._write_unsafe(lambda h: self._api.test_agent(email, _headers=h))

    def list_suppressions(
        self, email: str, *, limit: Optional[int] = None
    ) -> AutoPager[AgentSuppressionView]:
        """Beta: list recipient blocks scoped to this exact sending agent."""
        async def fetch(cursor: Optional[str]) -> Page:
            resp = await self._c._read(
                lambda h: self._api.list_agent_suppressions(
                    email, cursor=cursor, limit=limit, _headers=h
                )
            )
            return _page(resp.items, resp.next_cursor)

        return AutoPager(fetch)

    async def create_suppression(self, email: str, body: Body) -> AgentSuppressionView:
        """Beta: idempotently add a manual recipient block for this exact agent."""
        req = _coerce(CreateAgentSuppressionRequest, body)
        return await self._c._write_idempotent(
            lambda h: self._api.create_agent_suppression(email, req, _headers=h)
        )

    async def delete_suppression(
        self, email: str, address: str
    ) -> DeleteSuppressionResult:
        """Beta: remove only this exact agent-recipient block."""
        return await self._c._write_idempotent(
            lambda h: self._api.delete_agent_suppression(
                email, address, confirm="DELETE", _headers=h
            )
        )


class MessagesResource:
    """Message operations. The managed-unsubscribe option and its raw
    ``GET|POST /u/{token}`` confirmation flow are beta and may change before
    they are declared stable."""

    def __init__(self, api: MessagesApi, client: AsyncE2AClient) -> None:
        self._api = api
        self._c = client

    def list(
        self,
        email: str,
        *,
        direction: Optional[str] = None,
        read_status: Optional[str] = None,
        sort: Optional[str] = None,
        from_: Optional[str] = None,
        subject_contains: Optional[str] = None,
        conversation_id: Optional[str] = None,
        labels: Optional[List[str]] = None,
        q: Optional[str] = None,
        since: Optional[str] = None,
        until: Optional[str] = None,
        limit: Optional[int] = None,
        deleted: Optional[bool] = None,
    ) -> AutoPager[MessageSummaryView]:
        # `from` is a Python keyword; the generator is configured (via
        # --name-mappings/--parameter-name-mappings in generate-oag.sh) to expose
        # the idiomatic `from_` (PEP 8 trailing underscore) everywhere, so the
        # public surface and the generated base share one spelling.
        async def fetch(cursor: Optional[str]) -> Page:
            resp = await self._c._read(
                lambda h: self._api.list_messages(
                    email,
                    direction=direction,
                    read_status=read_status,
                    sort=sort,
                    from_=from_,
                    subject_contains=subject_contains,
                    conversation_id=conversation_id,
                    labels=labels,
                    q=q,
                    since=since,
                    until=until,
                    cursor=cursor,
                    limit=limit,
                    deleted=deleted,
                    _headers=h,
                )
            )
            return _page(resp.items, resp.next_cursor)

        return AutoPager(fetch)

    async def get(self, email: str, message_id: str) -> MessageView:
        return await self._c._read(lambda h: self._api.get_message(email, message_id, _headers=h))

    async def get_lifecycle(
        self,
        email: str,
        message_id: str,
        *,
        cursor: Optional[str] = None,
        limit: Optional[int] = None,
    ) -> PageMessageLifecycleTransition:
        """Beta: return the ordered observations e2a recorded for one message.

        The lifecycle contract may change before it is declared stable.
        """
        return await self._c._read(
            lambda h: self._api.get_message_lifecycle(
                email,
                message_id,
                cursor=cursor,
                limit=limit,
                _headers=h,
            )
        )

    async def delete(
        self, email: str, message_id: str, *, permanent: bool = False
    ) -> DeleteMessageResult:
        """Move a message to the trash.

        Reversible via ``restore()`` until the trash retention window expires
        (30 days by default), so the default soft delete needs no confirmation.

        Pass ``permanent=True`` to permanently delete a message that is ALREADY
        in the trash ("delete forever") — irreversible, account scope only. The
        typed .delete() call is the confirmation; the SDK supplies the
        ?confirm=DELETE guard the raw API requires on that path (it is ignored
        when permanent is unset).

        A message held for review cannot be deleted (409 message_held) — resolve
        it on the review queue first. Returns the deletion receipt
        ({deleted, id}).
        """
        return await self._c._write_idempotent(
            lambda h: self._api.delete_message(
                # `permanent or None` omits the param entirely on the soft path,
                # matching the TS SDK's wire shape (the server treats an absent
                # and a false `permanent` identically).
                email, message_id, permanent=permanent or None, confirm="DELETE", _headers=h
            )
        )

    async def restore(self, email: str, message_id: str) -> MessageView:
        """Restore a soft-deleted message and resume its retention clock."""
        return await self._c._write_unsafe(
            lambda h: self._api.restore_message(email, message_id, _headers=h)
        )

    async def get_attachment(
        self, email: str, message_id: str, index: int, *, inline: bool = False
    ) -> AttachmentView:
        # Metadata + a short-lived download_url (+ expires_at). inline=True also
        # returns base64 `data` for small attachments (<=256 KB; larger error).
        return await self._c._read(
            lambda h: self._api.get_attachment(email, message_id, index, inline=inline, _headers=h)
        )

    async def send(
        self,
        email: str,
        body: Body,
        *,
        unsubscribe: Optional[UnsubscribeInput] = None,
        wait: Optional[Literal["sent"]] = None,
        idempotency_key: Optional[str] = None,
    ) -> SendResultView:
        """Send a message. The optional managed-unsubscribe field is beta.

        Pass ``unsubscribe={"mode": "managed"}`` (or an
        :class:`UnsubscribeOptions`) to opt the message into e2a-managed
        unsubscribe handling; when given, it wins over any ``unsubscribe``
        already present in ``body``.

        Pass ``wait="sent"`` for an optional bounded wait: the request is held
        server-side until the asynchronously delivered message reaches a
        terminal-or-held state or at most 20 seconds elapse (currently ~15s), then returns the observed
        state; on timeout the result stays ``status="accepted"``. Default: no
        wait. Always branch on the result's ``status``, not the HTTP code.
        """
        req = _coerce(SendEmailRequest, body)
        if unsubscribe is not None:
            if req is body:
                # _coerce returns the caller's own model when body is already a
                # SendEmailRequest — copy before assigning so the kwarg doesn't
                # leak into the caller's object (and into later reuses of it).
                req = req.model_copy()
            req.unsubscribe = _coerce(UnsubscribeOptions, unsubscribe)
        return await self._c._write_keyed(
            lambda h: self._api.send_message(email, req, wait=wait, _headers=h),
            idempotency_key,
        )

    async def reply(
        self,
        email: str,
        message_id: str,
        body: Body,
        *,
        unsubscribe: Optional[UnsubscribeInput] = None,
        wait: Optional[Literal["sent"]] = None,
        idempotency_key: Optional[str] = None,
    ) -> SendResultView:
        """Reply to a message. The optional managed-unsubscribe field is beta,
        and ``wait="sent"`` requests the same bounded wait (see :meth:`send`)."""
        req = _coerce(ReplyRequest, body)
        if unsubscribe is not None:
            if req is body:
                req = req.model_copy()
            req.unsubscribe = _coerce(UnsubscribeOptions, unsubscribe)
        return await self._c._write_keyed(
            lambda h: self._api.reply_to_message(email, message_id, req, wait=wait, _headers=h),
            idempotency_key,
        )

    async def forward(
        self,
        email: str,
        message_id: str,
        body: Body,
        *,
        unsubscribe: Optional[UnsubscribeInput] = None,
        wait: Optional[Literal["sent"]] = None,
        idempotency_key: Optional[str] = None,
    ) -> SendResultView:
        """Forward a message. The optional managed-unsubscribe field is beta,
        and ``wait="sent"`` requests the same bounded wait (see :meth:`send`)."""
        req = _coerce(ForwardRequest, body)
        if unsubscribe is not None:
            if req is body:
                req = req.model_copy()
            req.unsubscribe = _coerce(UnsubscribeOptions, unsubscribe)
        return await self._c._write_keyed(
            lambda h: self._api.forward_message(email, message_id, req, wait=wait, _headers=h),
            idempotency_key,
        )

    # Approve/reject a held message live on the account-scoped review queue —
    # ``client.reviews.approve(message_id, body)`` /
    # ``client.reviews.reject(message_id, body)``. The deprecated per-inbox
    # messages.approve/reject was removed in the pre-GA vocabulary freeze (a
    # review is addressed by message id alone).

    async def update_labels(
        self, email: str, message_id: str, body: Body
    ) -> UpdateMessageResultView:
        req = _coerce(UpdateMessageRequest, body)
        return await self._c._write_idempotent(
            lambda h: self._api.update_message(email, message_id, req, _headers=h)
        )


class ReviewsResource:
    """The account-scoped human-review queue: every message held in
    pending_review (outbound drafts awaiting send approval + inbound messages
    held by a screening gate). Supersedes the per-inbox messages.approve/reject
    path — reviews are addressed by message id alone, no inbox email. Account-
    scoped credentials only; an agent cannot see or resolve holds."""

    def __init__(self, api: ReviewsApi, client: AsyncE2AClient) -> None:
        self._api = api
        self._c = client

    def list(self, *, limit: Optional[int] = None) -> AutoPager[ReviewView]:
        # Cursor-paginated: the AutoPager walks next_cursor to completion.
        async def fetch(cursor: Optional[str]) -> Page:
            resp = await self._c._read(lambda h: self._api.list_reviews(cursor=cursor, limit=limit, _headers=h))
            return _page(resp.items, resp.next_cursor)

        return AutoPager(fetch)

    async def get(self, message_id: str) -> MessageView:
        return await self._c._read(lambda h: self._api.get_review(message_id, _headers=h))

    async def approve(
        self,
        message_id: str,
        body: Optional[Body] = None,
        *,
        idempotency_key: Optional[str] = None,
    ) -> SendResultView:
        req = _coerce(ApproveRequest, body)
        return await self._c._write_keyed(
            lambda h: self._api.approve_review(message_id, req, _headers=h),
            idempotency_key,
        )

    async def reject(
        self, message_id: str, body: Optional[Body] = None
    ) -> RejectResultView:
        req = _coerce(RejectRequest, body)
        return await self._c._write_unsafe(
            lambda h: self._api.reject_review(message_id, req, _headers=h)
        )


class TemplatesResource:
    """Reusable email templates + the read-only starter catalog (beta — shapes
    may change before templates are declared stable). Account scope only; the
    send-side reference lives on ``messages.send`` (``template_id`` /
    ``template_alias`` / ``template_data``, mutually exclusive with a literal
    subject/body)."""

    def __init__(self, api: TemplatesApi, client: AsyncE2AClient) -> None:
        self._api = api
        self._c = client

    def list(self, *, limit: Optional[int] = None) -> AutoPager[TemplateSummaryView]:
        """List the account's stored templates, newest first. Summary rows only
        (no text/html sources) — ``get(id)`` returns the full sources."""

        # Cursor-paginated: the AutoPager walks next_cursor to completion.
        async def fetch(cursor: Optional[str]) -> Page:
            resp = await self._c._read(
                lambda h: self._api.list_templates(cursor=cursor, limit=limit, _headers=h)
            )
            return _page(resp.items, resp.next_cursor)

        return AutoPager(fetch)

    async def get(self, template_id: str) -> TemplateView:
        """Fetch one stored template by id (tmpl_…), including its sources."""
        return await self._c._read(lambda h: self._api.get_template(template_id, _headers=h))

    async def create(self, body: Body) -> TemplateView:
        """Create a template from literal source (name + subject + body), or copy
        a starter verbatim via ``from_starter`` (mutually exclusive with the
        source fields — edit the created copy afterwards with ``update``). Bare
        POST: not retried (mirrors agents/domains/webhooks create), since the
        create has no server-side idempotency dedup."""
        req = _coerce(CreateTemplateRequest, body)
        return await self._c._write_unsafe(lambda h: self._api.create_template(req, _headers=h))

    async def update(self, template_id: str, patch: Body) -> TemplateView:
        """Partial update; omitted fields are left unchanged. Changed parts are
        re-parsed. Set alias or html to "" to clear them. PATCH is
        idempotent → safe to retry."""
        req = _coerce(UpdateTemplateRequest, patch)
        return await self._c._write_idempotent(
            lambda h: self._api.update_template(template_id, req, _headers=h)
        )

    async def delete(self, template_id: str) -> DeleteTemplateResult:
        # In-flight sends are unaffected (rendering happens at send time). DELETE
        # is idempotent → safe to retry. Returns the deletion object ({deleted, id}).
        return await self._c._write_idempotent(lambda h: self._api.delete_template(template_id, confirm="DELETE", _headers=h))

    async def validate(self, body: Body) -> ValidateTemplateResponse:
        """Dry-run template source without persisting: per-part parse errors, a
        rendered preview against test_data (present only when valid), and
        suggested_data — a nested placeholder object covering every variable the
        source references. Side-effect-free → treated as a retryable read."""
        req = _coerce(ValidateTemplateRequest, body)
        return await self._c._read(lambda h: self._api.validate_template(req, _headers=h))

    def list_starters(self, *, limit: Optional[int] = None) -> AutoPager[StarterTemplateView]:
        """List the pre-built starter templates shipped with the deployment
        (catalog metadata + variables; ``get_starter(alias)`` adds the full body
        sources)."""

        # Cursor-paginated: the AutoPager walks next_cursor to completion.
        async def fetch(cursor: Optional[str]) -> Page:
            resp = await self._c._read(
                lambda h: self._api.list_starter_templates(cursor=cursor, limit=limit, _headers=h)
            )
            return _page(resp.items, resp.next_cursor)

        return AutoPager(fetch)

    async def get_starter(self, alias: str) -> StarterTemplateDetailView:
        """Fetch one starter by alias, including its full body sources. Starters
        are read-only masters — copy one with ``create({"from_starter": alias})``."""
        return await self._c._read(lambda h: self._api.get_starter_template(alias, _headers=h))


class ConversationsResource:
    def __init__(self, api: ConversationsApi, client: AsyncE2AClient) -> None:
        self._api = api
        self._c = client

    def list(
        self,
        email: str,
        *,
        since: Optional[str] = None,
        until: Optional[str] = None,
        limit: Optional[int] = None,
    ) -> AutoPager[ConversationSummaryView]:
        # Cursor-paginated (CV-3): the AutoPager walks next_cursor to completion.
        async def fetch(cursor: Optional[str]) -> Page:
            resp = await self._c._read(
                lambda h: self._api.list_conversations(
                    email, since=since, until=until, cursor=cursor, limit=limit, _headers=h
                )
            )
            return _page(resp.items, resp.next_cursor)

        return AutoPager(fetch)

    async def get(self, email: str, conversation_id: str) -> ConversationDetailView:
        return await self._c._read(
            lambda h: self._api.get_conversation(email, conversation_id, _headers=h)
        )


class DomainsResource:
    def __init__(self, api: DomainsApi, client: AsyncE2AClient) -> None:
        self._api = api
        self._c = client

    def list(self, *, limit: Optional[int] = None) -> AutoPager[DomainView]:
        # Cursor-paginated: the AutoPager walks next_cursor to completion.
        async def fetch(cursor: Optional[str]) -> Page:
            resp = await self._c._read(lambda h: self._api.list_domains(cursor=cursor, limit=limit, _headers=h))
            return _page(resp.items, resp.next_cursor)

        return AutoPager(fetch)

    async def get(self, domain: str) -> DomainView:
        return await self._c._read(lambda h: self._api.get_domain(domain, _headers=h))

    async def create(self, body: Body) -> DomainView:
        req = _coerce(RegisterDomainRequest, body)
        return await self._c._write_unsafe(lambda h: self._api.register_domain(req, _headers=h))

    async def delete(self, domain: str) -> DeleteDomainResult:
        # Returns the deletion object ({deleted, domain}).
        return await self._c._write_idempotent(lambda h: self._api.delete_domain(domain, confirm="DELETE", _headers=h))

    async def verify(self, domain: str) -> VerifyDomainView:
        return await self._c._write_unsafe(lambda h: self._api.verify_domain(domain, _headers=h))


class EventsResource:
    def __init__(self, api: EventsApi, client: AsyncE2AClient) -> None:
        self._api = api
        self._c = client

    def list(
        self,
        *,
        type: Optional[str] = None,
        agent_email: Optional[str] = None,
        conversation_id: Optional[str] = None,
        message_id: Optional[str] = None,
        since: Optional[str] = None,
        until: Optional[str] = None,
        limit: Optional[int] = None,
    ) -> AutoPager[EventView]:
        async def fetch(cursor: Optional[str]) -> Page:
            resp = await self._c._read(
                lambda h: self._api.list_events(
                    type=type,
                    agent_email=agent_email,
                    conversation_id=conversation_id,
                    message_id=message_id,
                    since=since,
                    until=until,
                    cursor=cursor,
                    limit=limit,
                    _headers=h,
                )
            )
            return _page(resp.items, resp.next_cursor)

        return AutoPager(fetch)

    async def get(self, event_id: str) -> EventView:
        return await self._c._read(lambda h: self._api.get_event(event_id, _headers=h))

    async def redeliver(self, event_id: str, body: Optional[Body] = None) -> RedeliverView:
        req = _coerce(RedeliverEventRequest, body)
        return await self._c._write_unsafe(
            lambda h: self._api.redeliver_event(event_id, req, _headers=h)
        )


class WebhooksResource:
    def __init__(self, api: WebhooksApi, client: AsyncE2AClient) -> None:
        self._api = api
        self._c = client

    async def fetch_message(self, event: EventLike) -> MessageView:
        """Fetch the full message referenced by an ``email.received`` event.

        Accepts any envelope-shaped event — a verified
        :class:`~e2a.v1.webhook_signature.WebhookEvent` or the WebSocket
        channel's :class:`~e2a.v1.websocket.WSEvent` (both channels carry the
        same envelope). The event is a metadata-only notification; this
        resolves its ``(delivered_to, message_id)`` fetch keys and returns the
        full :class:`MessageView` (body, attachments, and structured SMTP
        authentication evidence). Raises
        ``ValueError`` if the event is not an ``email.received`` carrying those
        keys.
        """
        data = event.data if isinstance(event.data, dict) else {}
        message_id = data.get("message_id")
        delivered_to = data.get("delivered_to")
        if event.type != "email.received" or not message_id or not delivered_to:
            raise ValueError(
                "fetch_message expects an email.received event with message_id and delivered_to"
            )
        return await self._c.messages.get(delivered_to, message_id)

    def list(self, *, limit: Optional[int] = None) -> AutoPager[WebhookView]:
        # Cursor-paginated: the AutoPager walks next_cursor to completion.
        async def fetch(cursor: Optional[str]) -> Page:
            resp = await self._c._read(lambda h: self._api.list_webhooks(cursor=cursor, limit=limit, _headers=h))
            return _page(resp.items, resp.next_cursor)

        return AutoPager(fetch)

    async def get(self, webhook_id: str) -> WebhookView:
        return await self._c._read(lambda h: self._api.get_webhook(webhook_id, _headers=h))

    async def create(
        self, body: Body, *, idempotency_key: Optional[str] = None
    ) -> CreateWebhookResponse:
        # Returns the one-time signing secret in `.signing_secret` — store it
        # now. Server-deduped via Idempotency-Key: a keyed retry replays the
        # same webhook (id + secret) instead of registering a second
        # subscription, so the SDK can safely retry an ambiguous transport
        # failure. A key is minted per call when not supplied, so intentional
        # duplicate subscriptions (even to the same URL) stay expressible.
        req = _coerce(CreateWebhookRequest, body)
        return await self._c._write_keyed(
            lambda h: self._api.create_webhook(req, _headers=h), idempotency_key
        )

    async def update(self, webhook_id: str, patch: Body) -> WebhookView:
        req = _coerce(UpdateWebhookRequest, patch)
        return await self._c._write_idempotent(
            lambda h: self._api.update_webhook(webhook_id, req, _headers=h)
        )

    async def delete(self, webhook_id: str) -> DeleteWebhookResult:
        # Returns the deletion object ({deleted, id}).
        return await self._c._write_idempotent(lambda h: self._api.delete_webhook(webhook_id, confirm="DELETE", _headers=h))

    async def rotate_secret(self, webhook_id: str) -> RotateSecretResponse:
        # Server-deduped via Idempotency-Key: a retried rotate replays the first
        # secret instead of minting a second. Mint a key + retry (parity with the
        # TS SDK, which retries rotate for the same reason).
        return await self._c._write_idempotent(
            lambda h: self._api.rotate_webhook_secret(webhook_id, _headers=h)
        )

    async def test(self, webhook_id: str, body: Optional[Body] = None) -> TestWebhookResponse:
        req = _coerce(TestWebhookRequest, body)
        return await self._c._write_unsafe(
            lambda h: self._api.test_webhook(webhook_id, req, _headers=h)
        )

    def deliveries(
        self, webhook_id: str, *, status: Optional[str] = None, limit: Optional[int] = None
    ) -> AutoPager[WebhookDeliveryView]:
        # Cursor-paginated: the AutoPager walks next_cursor to completion. The
        # status filter is pinned into the cursor server-side, which the pager
        # honors by keeping status constant across follow-up requests.
        async def fetch(cursor: Optional[str]) -> Page:
            resp = await self._c._read(
                lambda h: self._api.list_webhook_deliveries(
                    webhook_id, status=status, cursor=cursor, limit=limit, _headers=h
                )
            )
            return _page(resp.items, resp.next_cursor)

        return AutoPager(fetch)


class SuppressionsResource:
    def __init__(self, api: AccountApi, client: AsyncE2AClient) -> None:
        self._api = api
        self._c = client

    def list(self) -> AutoPager[SuppressionView]:
        # Cursor-paginated (A-5): walks next_cursor to completion.
        async def fetch(cursor: Optional[str]) -> Page:
            resp = await self._c._read(lambda h: self._api.list_suppressions(cursor=cursor, _headers=h))
            return _page(resp.items, resp.next_cursor)

        return AutoPager(fetch)

    async def delete(self, email: str) -> DeleteSuppressionResult:
        # Returns the deletion object ({deleted, address}).
        return await self._c._write_idempotent(lambda h: self._api.delete_suppression(email, confirm="DELETE", _headers=h))


class APIKeysResource:
    def __init__(self, api: AccountApi, client: AsyncE2AClient) -> None:
        self._api = api
        self._c = client

    def list(self, *, limit: Optional[int] = None) -> AutoPager[APIKeyView]:
        # Cursor-paginated: the AutoPager walks next_cursor to completion.
        async def fetch(cursor: Optional[str]) -> Page:
            resp = await self._c._read(lambda h: self._api.list_api_keys(cursor=cursor, limit=limit, _headers=h))
            return _page(resp.items, resp.next_cursor)

        return AutoPager(fetch)

    async def create(
        self, body: Body, *, idempotency_key: Optional[str] = None
    ) -> CreateAPIKeyResponse:
        # Returns the one-time plaintext key in `.key` — store it now. The
        # server replays the same credential for a keyed retry, so the SDK can
        # safely retry an ambiguous transport failure without minting twice.
        req = _coerce(CreateAPIKeyRequest, body)
        return await self._c._write_keyed(
            lambda h: self._api.create_api_key(req, _headers=h), idempotency_key
        )

    async def delete(self, key_id: str) -> DeleteApiKeyResult:
        # Returns the deletion object ({deleted, id}).
        return await self._c._write_idempotent(lambda h: self._api.delete_api_key(key_id, confirm="DELETE", _headers=h))


class AccountResource:
    def __init__(self, api: AccountApi, client: AsyncE2AClient) -> None:
        self._api = api
        self._c = client
        self.suppressions = SuppressionsResource(api, client)
        self.api_keys = APIKeysResource(api, client)

    async def get(self) -> AccountView:
        return await self._c._read(lambda h: self._api.get_account(_headers=h))

    async def export(self) -> UserExport:
        return await self._c._read(lambda h: self._api.export_account(_headers=h))

    async def delete(self) -> DeleteUserDataResult:
        # Deliberately NOT retried (unlike the other DELETEs): account deletion is
        # irreversible, so a transient failure should surface loudly to the caller
        # rather than silently re-firing. The typed .delete() call is the
        # confirmation; the SDK supplies the ?confirm=DELETE guard.
        return await self._c._write_unsafe(
            lambda h: self._api.delete_account(confirm="DELETE", _headers=h)
        )
