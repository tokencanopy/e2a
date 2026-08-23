"""Regression test for e2a#915: the dot-segment guard must cover the
generated-layer chokepoint, not just the 10 ergonomic call sites pinned in
``test_dot_segment_enumeration.py``.

That file already proves every ergonomic method (``client.agents.delete``,
``client.contacts.delete_outreach``, ...) rejects a dot-segment value before
any request is built. It cannot prove anything about a caller who imports a
generated ``*Api`` class directly and calls it against the SDK's own
``_TypedApiClient`` instance, skipping the ergonomic wrapper entirely,
which is exactly the gap #915 reports: the per-call-site
``_assert_not_dot_segment`` checks live in the ergonomic layer, not in the
one place (``ApiClient.param_serialize``) every generated method routes
through.
"""

from __future__ import annotations

import pytest

from e2a.v1 import AsyncE2AClient
from e2a.v1.errors import E2AValidationError
from e2a.v1.generated.api.agents_api import AgentsApi

BASE = "http://test.local"


@pytest.mark.anyio
async def test_generated_api_called_directly_still_rejects_dot_segment(httpx_mock):
    async with AsyncE2AClient(api_key="e2a_test", base_url=BASE) as c:
        # Bypasses AgentsResource (and its explicit _assert_not_dot_segment
        # call) entirely, but still goes through the SDK's _TypedApiClient.
        raw_api = AgentsApi(c._api_client)
        with pytest.raises(E2AValidationError) as ei:
            await raw_api.delete_agent_suppression(
                email="foo@example.com", address="..", confirm="DELETE"
            )
    assert ei.value.code == "unsafe_path_segment"
    assert httpx_mock.get_requests() == []


@pytest.mark.anyio
async def test_generated_api_called_directly_allows_ordinary_value(httpx_mock):
    httpx_mock.add_response(json={"address": "bar@example.com", "deleted": True})
    async with AsyncE2AClient(api_key="e2a_test", base_url=BASE) as c:
        raw_api = AgentsApi(c._api_client)
        result = await raw_api.delete_agent_suppression(
            email="foo@example.com", address="bar@example.com", confirm="DELETE"
        )
    assert result.deleted is True
