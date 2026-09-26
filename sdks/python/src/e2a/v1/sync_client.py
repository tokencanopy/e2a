"""The e2a synchronous client — a facade over :class:`AsyncE2AClient`.

Architecture (Playwright-style background event-loop bridge): a dedicated
daemon thread runs an asyncio event loop for the client's lifetime; the sync
client wraps an :class:`~e2a.v1.client.AsyncE2AClient` and every call submits
the underlying coroutine to that loop via
``asyncio.run_coroutine_threadsafe(coro, loop).result()``. There is exactly ONE
implementation of resources / retries / error mapping / pagination — the async
one — so the sync surface cannot drift from it.

Design decisions, documented here because they are deliberate:

- **The loop thread starts lazily on the first bridged call**, not in
  ``__init__``. Constructing a client at module import time (a common pattern)
  costs no thread, and a client created before a ``fork()`` (gunicorn prefork
  workers) doesn't strand a dead thread in the child — the worker spins its own
  loop on first use. The :class:`AsyncE2AClient` itself IS constructed eagerly
  in ``__init__`` (it performs no I/O and touches no event loop), so
  constructor validation such as a missing API key raises immediately.
- **Resource namespaces are mirrored dynamically, never hand-copied.**
  ``__getattr__`` on the client / resource proxies inspects the async
  attribute: coroutine functions become blocking callables, methods returning
  an :class:`AutoPager` yield a :class:`SyncAutoPager`, async-iterables (the WS
  stream) yield a :class:`SyncStream`, and nested resource objects yield
  another proxy. Adding a resource or method to the async client makes it
  appear here automatically.
- **Typing**: the dynamic proxy is typed as ``Any`` (``__getattr__ -> Any``),
  so attribute access type-checks without generated stubs. Full static stubs
  for the mirrored tree are intentionally out of scope for now.
- **Async-context guard**: calling a sync method from inside a RUNNING event
  loop would deadlock-or-block the loop, so the submit path raises a guiding
  ``RuntimeError`` instead ("… use AsyncE2AClient").
- **Error transparency**: ``concurrent.futures.Future.result()`` re-raises the
  exception the coroutine raised, unwrapped — ``except E2ALimitExceededError:``
  works identically in sync code.
- **Shutdown**: ``close()`` (idempotent; also the context-manager ``__exit__``)
  closes the async client on the loop, then stops the loop thread — cancelling
  any still-pending task so a blocked ``listen()`` iteration unblocks and ends
  cleanly. A ``weakref.finalize`` hook (which also runs at interpreter exit)
  plus the daemon flag on the thread guarantee an unclosed client never hangs
  interpreter shutdown.
"""

from __future__ import annotations

import asyncio
import concurrent.futures
import functools
import inspect
import threading
import weakref
from typing import Any, Callable, Coroutine, Generic, Iterator, List, Optional, TypeVar

from .client import AsyncE2AClient, async_signup_agent
from .errors import E2AError
from .pagination import AutoPager, Page
from .inbound import AsyncInboundResource, InboundResource

__all__ = ["E2AClient", "SyncAutoPager", "SyncStream", "signup_agent"]

T = TypeVar("T")

_ASYNC_CONTEXT_MESSAGE = (
    "E2AClient (sync) called from inside a running event loop — "
    "use AsyncE2AClient in async code"
)

#: Async-lifecycle members that must not leak onto the sync surface — the sync
#: client's lifecycle is close()/with, not aclose()/async with.
_EXCLUDED_ATTRS = frozenset({"aclose"})


def signup_agent(body: Any, **kwargs: Any) -> Any:
    """Synchronous public agent-signup entry point; no API key required."""
    try:
        asyncio.get_running_loop()
    except RuntimeError:
        return asyncio.run(async_signup_agent(body, **kwargs))
    raise RuntimeError(
        "signup_agent (sync) called from inside a running event loop — "
        "use async_signup_agent in async code"
    )


def _closed_error() -> E2AError:
    return E2AError(
        code="client_closed",
        message="E2AClient is closed — create a new client",
        status=0,
        retryable=False,
    )


class _EventLoopBridge:
    """Owns the background event loop: a daemon thread started lazily on the
    first submitted coroutine, torn down by :meth:`close`."""

    def __init__(self) -> None:
        self._loop: Optional[asyncio.AbstractEventLoop] = None
        self._thread: Optional[threading.Thread] = None
        self._lock = threading.Lock()
        self._closed = False

    @property
    def closed(self) -> bool:
        return self._closed

    # ── loop lifecycle ───────────────────────────────────────────────
    def _ensure_loop(self) -> asyncio.AbstractEventLoop:
        loop = self._loop
        if loop is not None:
            return loop
        with self._lock:
            if self._closed:
                raise _closed_error()
            if self._loop is None:
                loop = asyncio.new_event_loop()
                started = threading.Event()

                def run() -> None:
                    asyncio.set_event_loop(loop)
                    started.set()
                    try:
                        loop.run_forever()
                    finally:
                        try:
                            # Cancel whatever close() left pending (e.g. a
                            # blocked WS read) so their waiters unblock, then
                            # let the cancellations run to completion.
                            tasks = asyncio.all_tasks(loop)
                            for t in tasks:
                                t.cancel()
                            if tasks:
                                loop.run_until_complete(
                                    asyncio.gather(*tasks, return_exceptions=True)
                                )
                            loop.run_until_complete(loop.shutdown_asyncgens())
                        finally:
                            loop.close()

                thread = threading.Thread(target=run, name="e2a-sync-client", daemon=True)
                thread.start()
                started.wait()
                self._thread = thread
                self._loop = loop
            return self._loop

    # ── the submit path ──────────────────────────────────────────────
    def submit(self, coro: Coroutine[Any, Any, T]) -> T:
        """Run ``coro`` on the background loop and block for its result.

        Raises the coroutine's own exception unwrapped (typed E2A errors
        propagate as themselves). Guards against being called from inside a
        running event loop, and against a closed client.
        """
        try:
            asyncio.get_running_loop()
        except RuntimeError:
            pass  # no running loop in this thread — the normal sync case
        else:
            coro.close()  # avoid the "coroutine was never awaited" warning
            raise RuntimeError(_ASYNC_CONTEXT_MESSAGE)
        if self._closed:
            coro.close()
            raise _closed_error()
        loop = self._ensure_loop()
        future = asyncio.run_coroutine_threadsafe(coro, loop)
        try:
            return future.result()
        # NB: catch BOTH cancellation classes — they were unified in 3.8 but
        # concurrent.futures.CancelledError is a distinct class again on some
        # versions (observed on 3.14), and future.result() raises that one.
        except (asyncio.CancelledError, concurrent.futures.CancelledError):
            if self._closed:
                # close() tore the loop down while we were blocked (e.g. a
                # pending listen() read) — surface a typed, recognizable error;
                # SyncStream turns it into a clean end-of-iteration.
                raise _closed_error() from None
            raise

    # ── teardown ─────────────────────────────────────────────────────
    def close(
        self,
        shutdown: Optional[Callable[[], Coroutine[Any, Any, Any]]] = None,
        *,
        timeout: float = 10.0,
    ) -> None:
        """Mark closed, run ``shutdown()`` (the async client's ``aclose``) on
        the loop, then stop and join the loop thread. Idempotent; safe to call
        from any thread (including the GC/atexit finalizer)."""
        with self._lock:
            if self._closed:
                return
            self._closed = True
            loop, thread = self._loop, self._thread

        in_running_loop = True
        try:
            asyncio.get_running_loop()
        except RuntimeError:
            in_running_loop = False

        if loop is None:
            # The loop never started, so no request was ever made — the async
            # client holds no open connections. Still run aclose() for
            # symmetry, on a throwaway loop (skipped if we're inside someone
            # else's running loop, where a blocking asyncio.run is illegal).
            if shutdown is not None:
                coro = shutdown()
                if in_running_loop:
                    coro.close()
                else:
                    try:
                        asyncio.run(coro)
                    except Exception:  # pragma: no cover - defensive
                        pass
            return

        if shutdown is not None:
            try:
                fut = asyncio.run_coroutine_threadsafe(shutdown(), loop)
                if in_running_loop:
                    # Called from async code (or a finalizer inside one): don't
                    # block the caller's loop. The shutdown task is drained (or
                    # cancelled) by the loop thread's teardown below.
                    pass
                else:
                    fut.result(timeout)
            except Exception:  # pragma: no cover - defensive teardown
                pass
        try:
            loop.call_soon_threadsafe(loop.stop)
        except RuntimeError:  # pragma: no cover - loop already gone
            pass
        if thread is not None and thread is not threading.current_thread():
            thread.join(timeout)


# ── dynamic wrapping ─────────────────────────────────────────────────


def _wrap_attr(attr: Any, bridge: _EventLoopBridge) -> Any:
    """Map one attribute of the async tree onto the sync surface."""
    if inspect.iscoroutinefunction(attr):

        @functools.wraps(attr)
        def call_blocking(*args: Any, **kwargs: Any) -> Any:
            return bridge.submit(attr(*args, **kwargs))

        return call_blocking
    if callable(attr):
        # Plain (non-async) methods — e.g. `.list()` building an AutoPager, or
        # `.listen()` building a WSStream. Call through, then bridge the result.

        @functools.wraps(attr)
        def call_through(*args: Any, **kwargs: Any) -> Any:
            return _wrap_value(attr(*args, **kwargs), bridge)

        return call_through
    return _wrap_value(attr, bridge)


def _wrap_value(value: Any, bridge: _EventLoopBridge) -> Any:
    if isinstance(value, AsyncInboundResource):
        return InboundResource(value, bridge)
    if isinstance(value, AutoPager):
        return SyncAutoPager(value, bridge)
    if inspect.iscoroutine(value):  # defensive: a plain method returned a coroutine
        return bridge.submit(value)
    if hasattr(type(value), "__aiter__"):  # WSStream (or any async-iterable)
        return SyncStream(value, bridge)
    if _is_resource(value):
        return _SyncResourceProxy(value, bridge)
    return value


def _is_resource(value: Any) -> bool:
    """A nested resource namespace (e.g. ``account.suppressions``): a
    non-callable SDK-defined object that is not a data model."""
    if callable(value) or isinstance(value, (str, bytes, int, float, bool, type(None))):
        return False
    cls = type(value)
    if not cls.__module__.startswith("e2a."):
        return False
    # Pydantic models are data, not namespaces — pass them through untouched.
    return not hasattr(cls, "model_fields")


class _SyncResourceProxy:
    """Sync facade over one async resource object; nested resources get their
    own proxy. Wrapped attributes are cached in ``__dict__`` after first use
    (``__getattr__`` only fires on misses)."""

    def __init__(self, target: Any, bridge: _EventLoopBridge) -> None:
        self._target = target
        self._bridge = bridge

    def __getattr__(self, name: str) -> Any:
        if name.startswith("_"):
            # NB: don't touch self._target here — this branch also guards
            # against pre-__init__ lookups (copy/pickle), where it isn't set.
            raise AttributeError(f"{type(self).__name__!r} has no attribute {name!r}")
        wrapped = _wrap_attr(getattr(self._target, name), self._bridge)
        self.__dict__[name] = wrapped
        return wrapped

    def __dir__(self) -> List[str]:
        return sorted(
            {n for n in dir(self._target) if not n.startswith("_")}
            | set(type(self).__dict__)
        )

    def __repr__(self) -> str:
        return f"<sync proxy of {self._target!r}>"


class SyncAutoPager(Generic[T]):
    """Synchronous view of an :class:`~e2a.v1.pagination.AutoPager`.

    Iterate it directly (``for item in client.agents.list(): ...``) — each item
    is pulled from the async pager through the bridge, so cursor threading and
    the anti-loop guards are the async implementation's. Also mirrors the async
    pager's ``page`` / ``to_list`` / ``for_each``.
    """

    def __init__(self, pager: AutoPager[T], bridge: _EventLoopBridge) -> None:
        self._pager = pager
        self._bridge = bridge

    def __iter__(self) -> Iterator[T]:
        ait = self._pager.__aiter__()
        try:
            while True:
                try:
                    item = self._bridge.submit(ait.__anext__())
                except StopAsyncIteration:
                    return
                yield item
        finally:
            # The sync generator was closed early (break/GC) — release the
            # suspended async generator on the loop.
            if not self._bridge.closed:
                try:
                    self._bridge.submit(ait.aclose())
                except Exception:  # pragma: no cover - defensive cleanup
                    pass

    def page(self, cursor: Optional[str] = None) -> Page[T]:
        """Fetch a SINGLE page for caller-driven pagination — pass the previous
        page's ``next_cursor`` (omit for the first page). See
        :meth:`AutoPager.page`."""
        return self._bridge.submit(self._pager.page(cursor))

    def to_list(self, *, limit: int) -> List[T]:
        """Collect up to ``limit`` items (the limit is required, mirroring the
        async pager — it caps memory for an inbox that could page forever)."""
        return self._bridge.submit(self._pager.to_list(limit=limit))

    def for_each(self, fn: Callable[[T], Optional[bool]]) -> None:
        """Invoke ``fn`` per item; return ``False`` from ``fn`` to stop early."""
        for item in self:
            if fn(item) is False:
                return


class SyncStream(Generic[T]):
    """Synchronous view of an async-iterable stream (``client.listen(...)``).

    Each item is bridged through the loop. ``client.close()`` unblocks a
    pending iteration and ends it cleanly (``StopIteration``), so a
    ``for notif in client.listen(...)`` loop exits instead of raising.
    """

    def __init__(self, stream: Any, bridge: _EventLoopBridge) -> None:
        self._stream = stream
        self._bridge = bridge

    def __iter__(self) -> Iterator[T]:
        ait = self._stream.__aiter__()
        try:
            while True:
                try:
                    item = self._bridge.submit(ait.__anext__())
                except StopAsyncIteration:
                    return
                except E2AError as e:
                    if e.code == "client_closed":
                        return  # close() unblocked us — end the stream cleanly
                    raise
                yield item
        finally:
            aclose = getattr(ait, "aclose", None)
            if aclose is not None and not self._bridge.closed:
                try:
                    self._bridge.submit(aclose())
                except Exception:  # pragma: no cover - defensive cleanup
                    pass


def _finalize(bridge: _EventLoopBridge, async_client: AsyncE2AClient) -> None:
    """GC / interpreter-exit fallback for an unclosed client (also the body of
    ``close()``). Must never raise and must not reference the E2AClient."""
    try:
        bridge.close(async_client.aclose)
    except Exception:  # pragma: no cover - defensive teardown
        pass


class E2AClient:
    """Synchronous client for the e2a /v1 API.

    A facade over :class:`AsyncE2AClient` (see the module docstring for the
    bridge architecture): identical constructor, resources, retry/idempotency
    behavior, typed errors, and pagination semantics — minus the ``await``.

    Use as a context manager (or call :meth:`close`) so the background loop
    thread and HTTP connections are released::

        with E2AClient(api_key="e2a_...") as client:
            for agent in client.agents.list():
                print(agent.email)

    Must not be used from async code — sync calls made while an event loop is
    running in the current thread raise ``RuntimeError``; use
    :class:`AsyncE2AClient` there.
    """

    def __init__(
        self,
        api_key: Optional[str] = None,
        *,
        base_url: Optional[str] = None,
        max_retries: int = 2,
        max_elapsed_ms: Optional[float] = None,
        timeout_ms: Optional[float] = 30_000.0,
        _retry_config: Optional[Any] = None,
    ) -> None:
        # Mirrors AsyncE2AClient.__init__ exactly (a signature-parity test
        # enforces this). Constructing the async client is pure setup — no I/O,
        # no event loop — so it's safe off-loop and validates eagerly.
        self._bridge = _EventLoopBridge()
        self._async_client = AsyncE2AClient(
            api_key,
            base_url=base_url,
            max_retries=max_retries,
            max_elapsed_ms=max_elapsed_ms,
            timeout_ms=timeout_ms,
            _retry_config=_retry_config,
        )
        self._finalizer = weakref.finalize(self, _finalize, self._bridge, self._async_client)

    # ── dynamic mirror of the async surface ─────────────────────────
    def __getattr__(self, name: str) -> Any:
        if name.startswith("_"):
            raise AttributeError(f"{type(self).__name__!r} object has no attribute {name!r}")
        if name in _EXCLUDED_ATTRS:
            raise AttributeError(
                f"{name!r} is not available on the sync E2AClient — use close() "
                "(or a `with` block); AsyncE2AClient has the async lifecycle"
            )
        wrapped = _wrap_attr(getattr(self._async_client, name), self._bridge)
        self.__dict__[name] = wrapped  # cache: __getattr__ only fires on misses
        return wrapped

    def __dir__(self) -> List[str]:
        mirrored = {
            n
            for n in dir(self._async_client)
            if not n.startswith("_") and n not in _EXCLUDED_ATTRS
        }
        return sorted(mirrored | {n for n in super().__dir__() if not n.startswith("_")})

    # ── lifecycle ────────────────────────────────────────────────────
    def close(self) -> None:
        """Close the underlying async client and stop the loop thread.

        Idempotent (double-close is a no-op) and unblocks any iteration
        pending on ``listen()``. After close, API calls raise a typed
        ``E2AError`` with ``code="client_closed"``.
        """
        # weakref.finalize invokes the callback at most once — this is what
        # makes close() / GC / interpreter-exit race-free among themselves.
        self._finalizer()

    def __enter__(self) -> "E2AClient":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()
