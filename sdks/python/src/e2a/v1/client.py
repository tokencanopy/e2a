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
from datetime import datetime
from typing import Any, Awaitable, Callable, List, Literal, Mapping, Optional, Protocol, Sequence, Tuple, Type, TypeVar, Union

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
from .generated.api.contacts_api import ContactsApi
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
    ForwardRequestReplyTo,
    AccountView,
    AttachmentView,
    MessageSummaryView,
    AccountMetricsView,
    AgentMetricsView,
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
    ContactView,
    CreateContactRequest,
    UpdateContactRequest,
    DeleteContactResult,
    ImportContactsRequest,
    ContactImportResult,
    DeleteImportBatchResult,
    ContactEngagementView,
    UpsertEngagementRequest,
    DeleteEngagementResult,
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


def _reply_to_union(body: Optional[Body]) -> Optional[Body]:
    """Wrap a raw ``reply_to`` on a dict body in the generated oneOf union.

    Reply-To is an RFC 5322 address-list: the API accepts one address or a list
    of up to five. The generated request models type ``reply_to`` as the
    ``ForwardRequestReplyTo`` oneOf, which pydantic will NOT build from a bare
    ``str``/``list`` when :func:`_coerce` validates the body — so wrap those here
    so both ``{"reply_to": "a@x"}`` (the historical single-address form) and
    ``{"reply_to": ["a@x", "b@y"]}`` keep working. Non-dict bodies and values
    that are already a ``ForwardRequestReplyTo`` (or ``None``) pass through
    unchanged. The body dict is copied, never mutated in place.
    """
    if isinstance(body, dict):
        rt = body.get("reply_to")
        if isinstance(rt, (str, list)):
            return {**body, "reply_to": ForwardRequestReplyTo(actual_instance=rt)}
    return body


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
        self.contacts = ContactsResource(ContactsApi(self._api_client), self)
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


def _header(headers: Optional[Mapping[str, str]], name: str) -> Optional[str]:
    """Read a response header without relying on transport-specific casing."""
    if not headers:
        return None
    wanted = name.lower()
    for key, value in headers.items():
        if key.lower() == wanted:
            return value
    return None


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
        """Restore an agent from the 30-day trash. Scheduled messages restored
        before scheduled_at re-arm; at/after scheduled_at they return live as
        failed with submission canceled. Account scope only."""
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
    """Message operations.

    Scheduled sending via ``send_at`` / ``scheduled_at`` and the managed-
    unsubscribe option (including its raw ``GET|POST /u/{token}`` confirmation
    flow) are beta and may change before they are declared stable.
    """

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
        since: Optional[str] = None,
        until: Optional[str] = None,
        limit: Optional[int] = None,
        deleted: Optional[bool] = None,
        filter: Optional[str] = None,
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
                    since=since,
                    until=until,
                    cursor=cursor,
                    limit=limit,
                    deleted=deleted,
                    filter=filter,
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

    async def get_metrics(
        self,
        email: str,
        *,
        start: Optional[datetime] = None,
        end: Optional[datetime] = None,
    ) -> AgentMetricsView:
        """Beta: counter metrics for one agent over a cohort window.

        Messages belong to the window by their own creation time, not by when
        each observation landed, so a rate never mixes numerator and
        denominator from different populations. Bounce and complaint feedback
        keeps arriving for up to 72 hours, so treat the most recent days as
        provisional.

        Every field of ``rates`` is ``None`` — never ``0`` — when its
        denominator is zero, so "no traffic" stays distinguishable from
        "everything failed".

        Defaults to the last 30 days; the window may not exceed 92 days.
        """
        return await self._c._read(
            lambda h: self._api.get_agent_metrics(
                email,
                start=start,
                end=end,
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
        """Restore a soft-deleted message. A scheduled message restored before
        scheduled_at re-arms; at/after scheduled_at it returns live as failed
        with submission canceled."""
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
        """Send a message.

        Scheduled sending via ``body.send_at`` and the resulting
        ``scheduled_at`` / ``status="scheduled"`` fields is beta and may
        change before it is declared stable. The optional managed-unsubscribe
        field is also beta.

        Pass ``unsubscribe={"mode": "managed"}`` (or an
        :class:`UnsubscribeOptions`) to opt the message into e2a-managed
        unsubscribe handling; when given, it wins over any ``unsubscribe``
        already present in ``body``.

        Pass ``wait="sent"`` for an optional bounded wait on an immediate
        send: the request is held server-side until the asynchronously
        delivered message reaches a terminal-or-held state or at most 20
        seconds elapse (currently ~15s), then returns the observed state; on
        timeout the result stays ``status="accepted"``. A future ``send_at``
        returns ``status="scheduled"`` immediately and does not wait for that
        time. Default: no wait. Always branch on the result's ``status``, not
        the HTTP code.
        """
        req = _coerce(SendEmailRequest, _reply_to_union(body))
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
        quote_history: Optional[bool] = None,
        wait: Optional[Literal["sent"]] = None,
        idempotency_key: Optional[str] = None,
    ) -> SendResultView:
        """Reply to a message.

        Scheduled sending via ``body.send_at`` is beta and may change before
        it is declared stable. The optional managed-unsubscribe field is also
        beta, and ``wait="sent"`` requests the same bounded wait (see
        :meth:`send`).

        Beta: pass ``quote_history=True`` to have the server append
        the referenced message as mail-client-style quoted history beneath
        the reply body (an attribution line plus the '>'-quoted text, and a
        blockquote when an HTML body is supplied); when given, it wins over
        any ``quote_history`` already present in ``body``. This option may
        change before it is declared stable.
        """
        req = _coerce(ReplyRequest, _reply_to_union(body))
        if unsubscribe is not None:
            if req is body:
                req = req.model_copy()
            req.unsubscribe = _coerce(UnsubscribeOptions, unsubscribe)
        if quote_history is not None:
            if req is body:
                req = req.model_copy()
            req.quote_history = quote_history
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
        """Forward a message.

        Scheduled sending via ``body.send_at`` is beta and may change before
        it is declared stable. The optional managed-unsubscribe field is also
        beta, and ``wait="sent"`` requests the same bounded wait (see
        :meth:`send`).
        """
        req = _coerce(ForwardRequest, _reply_to_union(body))
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


class ContactsResource:
    """The people this account corresponds with (beta — shapes may change before
    contacts are declared stable). Account scope only.

    Contact identity is account-level on purpose: the same person may be worked
    by more than one agent, and duplicating them per agent would make
    "has anyone on our side already contacted this fund?" unanswerable."""

    def __init__(self, api: ContactsApi, client: AsyncE2AClient) -> None:
        self._api = api
        self._c = client

    def list(
        self,
        *,
        source: Optional[str] = None,
        import_batch_id: Optional[str] = None,
        created_after: Optional[datetime] = None,
        created_before: Optional[datetime] = None,
        limit: Optional[int] = None,
    ) -> AutoPager[ContactView]:
        """List contacts, newest first. Optionally narrow by provenance,
        upload, or creation-time window."""

        # Cursor-paginated: the AutoPager walks next_cursor to completion.
        async def fetch(cursor: Optional[str]) -> Page:
            resp = await self._c._read(
                lambda h: self._api.list_contacts(
                    source=source, import_batch_id=import_batch_id,
                    created_after=created_after, created_before=created_before,
                    cursor=cursor, limit=limit, _headers=h,
                )
            )
            return _page(resp.items, resp.next_cursor)

        return AutoPager(fetch)

    async def get(self, address: str) -> ContactView:
        """Fetch one contact. ``address`` may be bare or a display-name form
        ("A. Partner <partner@fund.vc>") — both resolve to the same contact."""
        return await self._c._read(lambda h: self._api.get_contact(address, _headers=h))

    async def get_with_etag(self, address: str) -> Tuple[ContactView, Optional[str]]:
        """Fetch one contact and its optimistic-concurrency validator. Pass the
        returned ETag to :meth:`update` as ``if_match`` to reject stale edits."""
        response = await self._c._read(
            lambda h: self._api.get_contact_with_http_info(address, _headers=h)
        )
        return response.data, _header(response.headers, "etag")

    async def create(
        self, body: Body, *, idempotency_key: Optional[str] = None
    ) -> ContactView:
        """Create one contact. The address is canonicalized, so creating the same
        person twice in any form is a conflict rather than a duplicate row."""
        req = _coerce(CreateContactRequest, body)
        return await self._c._write_keyed(
            lambda h: self._api.create_contact(req, _headers=h),
            idempotency_key,
        )

    async def update(
        self, address: str, patch: Body, *, if_match: Optional[str] = None
    ) -> ContactView:
        """Partial update; omitted fields are left unchanged, so editing the name
        never erases metadata. Address and provenance are immutable."""
        req = _coerce(UpdateContactRequest, patch)
        return await self._c._write_idempotent(
            lambda h: self._api.update_contact(
                address, req, if_match=if_match, _headers=h
            )
        )

    async def delete(self, address: str) -> DeleteContactResult:
        """Remove a contact. Suppressions are untouched — consent outlives the
        record, so this never makes a blocked address sendable again."""
        return await self._c._write_idempotent(
            lambda h: self._api.delete_contact(address, confirm="DELETE", _headers=h)
        )

    async def import_(
        self, body: Body, *, idempotency_key: Optional[str] = None
    ) -> ContactImportResult:
        """Import up to 1000 contacts in one request. Every submitted row gets its
        own result, so one bad line never rejects the upload. Import is inert — it
        records identity and sends nothing. A row that omits a field keeps the
        stored value, so a narrower re-upload does not erase columns it no longer
        carries."""
        req = _coerce(ImportContactsRequest, body)
        return await self._c._write_keyed(
            lambda h: self._api.import_contacts(req, _headers=h),
            idempotency_key,
        )

    async def delete_import(self, batch_id: str) -> DeleteImportBatchResult:
        """Reverse an import, removing untouched contacts and agent enrolments it created."""
        return await self._c._write_idempotent(
            lambda h: self._api.delete_import_batch(batch_id, confirm="DELETE", _headers=h)
        )

    # ── Per-agent outreach ───────────────────────────────────────────────────
    # Engagements are one agent's relationship with a contact. Unlike the
    # account-level methods above, an agent-scoped credential may drive these
    # for its own agent — that is the outreach loop.

    def outreach(
        self,
        email: str,
        *,
        stage: Optional[str] = None,
        replied: Optional[bool] = None,
        suppressed: Optional[bool] = None,
        next_action_before: Optional[datetime] = None,
        last_outbound_before: Optional[datetime] = None,
        limit: Optional[int] = None,
    ) -> AutoPager[ContactEngagementView]:
        """List the contacts an agent is working, with the reply and delivery
        facts e2a derives from real message activity.

        For a follow-up sweep pass ``replied=False`` together with BOTH
        ``next_action_before`` and ``last_outbound_before``. ``last_outbound_at``
        is server-maintained, so including it drops anyone just contacted even
        if your own state write was lost — omit it and a failed write can send
        the same person twice."""

        def _flag(v: Optional[bool]) -> Optional[str]:
            return None if v is None else ("true" if v else "false")

        # Cursor-paginated: the AutoPager walks next_cursor to completion.
        async def fetch(cursor: Optional[str]) -> Page:
            resp = await self._c._read(
                lambda h: self._api.list_engagements(
                    email, stage=stage, replied=_flag(replied),
                    suppressed=_flag(suppressed),
                    next_action_before=next_action_before,
                    last_outbound_before=last_outbound_before,
                    cursor=cursor, limit=limit, _headers=h,
                )
            )
            return _page(resp.items, resp.next_cursor)

        return AutoPager(fetch)

    async def get_outreach(self, email: str, address: str) -> ContactEngagementView:
        """Fetch one agent's outreach record for a contact."""
        return await self._c._read(lambda h: self._api.get_engagement(email, address, _headers=h))

    async def get_outreach_with_etag(
        self, email: str, address: str
    ) -> Tuple[ContactEngagementView, Optional[str]]:
        """Fetch one outreach record and its validator for a guarded
        read-modify-write loop."""
        response = await self._c._read(
            lambda h: self._api.get_engagement_with_http_info(
                email, address, _headers=h
            )
        )
        return response.data, _header(response.headers, "etag")

    async def set_outreach(
        self, email: str, address: str, body: Body, *, if_match: Optional[str] = None
    ) -> ContactEngagementView:
        """Enrol a contact in an agent's outreach, or update the agent-owned
        fields. Omitted fields are left unchanged, so advancing the stage after a
        send does not disturb the schedule."""
        req = _coerce(UpsertEngagementRequest, body)
        return await self._c._write_idempotent(
            lambda h: self._api.upsert_engagement(
                email, address, req, if_match=if_match, _headers=h
            )
        )

    async def delete_outreach(self, email: str, address: str) -> DeleteEngagementResult:
        """Un-enrol a contact from an agent's outreach. The contact itself
        survives, and suppressions are untouched — this is not consent."""
        return await self._c._write_idempotent(
            lambda h: self._api.delete_engagement(email, address, confirm="DELETE", _headers=h)
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
        # Only sending_teardown="confirmed" permits DNS removal. Repeating
        # this call after a lost response polls the durable teardown receipt.
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

    def list(self, *, limit: Optional[int] = None) -> AutoPager[SuppressionView]:
        # Cursor-paginated (A-5): walks next_cursor to completion.
        async def fetch(cursor: Optional[str]) -> Page:
            resp = await self._c._read(lambda h: self._api.list_suppressions(cursor=cursor, limit=limit, _headers=h))
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

    async def metrics(
        self,
        *,
        start: Optional[datetime] = None,
        end: Optional[datetime] = None,
        group_by: Optional[str] = None,
        bucket: Optional[str] = None,
    ) -> AccountMetricsView:
        """Beta: counter metrics across every agent this account owns.

        Uses the same cohort-window and denominator contract as
        ``messages.get_metrics()``, so an account total and the per-agent
        numbers under it can never disagree about what a rate means.

        Pass ``bucket="day"`` for per-day buckets suitable for charting;
        they are gap-filled so a silent day is present with zeroes rather
        than missing.

        Pass ``group_by="agent"`` for a per-agent breakdown, busiest first. It
        is capped at 200 agents and sets ``agents_truncated`` when it cuts; the
        totals stay complete regardless.

        Account-scoped credentials only — an agent-scoped key reads its own
        agent through ``messages.get_metrics()`` instead.
        """
        return await self._c._read(
            lambda h: self._api.get_account_metrics(
                start=start,
                end=end,
                bucket=bucket,
                group_by=group_by,
                _headers=h,
            )
        )

    async def delete(self) -> DeleteUserDataResult:
        # Deliberately NOT retried (unlike the other DELETEs): account deletion is
        # irreversible, so a transient failure should surface loudly to the caller
        # rather than silently re-firing. The typed .delete() call is the
        # confirmation; the SDK supplies the ?confirm=DELETE guard.
        return await self._c._write_unsafe(
            lambda h: self._api.delete_account(confirm="DELETE", _headers=h)
        )
