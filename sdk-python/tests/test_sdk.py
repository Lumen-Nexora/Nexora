from __future__ import annotations

import asyncio
import json
from pathlib import Path
from typing import Any

import httpx
import pytest

from nexora import NexoraClient, NexoraError, RequestOptions
from nexora.errors import ValidationError


ROOT = Path(__file__).resolve().parents[2]
CONFORMANCE = json.loads((ROOT / "sdk" / "conformance" / "cases.json").read_text())["cases"]


@pytest.mark.parametrize("case", CONFORMANCE, ids=[case["name"] for case in CONFORMANCE])
def test_shared_conformance(case: dict[str, Any]) -> None:
    calls = 0
    keys: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        keys.append(request.headers.get("Idempotency-Key", ""))
        status = case["statuses"][min(calls - 1, len(case["statuses"]) - 1)]
        headers = {"Retry-After": case["retry_after"]} if calls == 1 and case.get("retry_after") else {}
        if status >= 400:
            return httpx.Response(status, headers=headers, json={"error": {"code": "TEMPORARY", "message": "retry"}})
        return httpx.Response(status, headers=headers, json={"ok": True})

    async def run() -> None:
        client = NexoraClient(
            "test-key",
            base_url="https://api.example.test",
            max_retries=case.get("max_retries", 0),
            retry_delay=0,
            transport=httpx.MockTransport(handler),
        )
        options = RequestOptions(idempotency_key=case.get("idempotency_key"))
        error: NexoraError | None = None
        try:
            await client._http.request(case["method"], case["path"], options=options)
        except NexoraError as exc:
            error = exc
        finally:
            await client.aclose()
        assert (error is not None) is case.get("expected_error", False)

    asyncio.run(run())
    assert calls == case["expected_requests"]
    if case["key_mode"] == "generated":
        assert keys and keys[0] and len(set(keys)) == 1
    elif case["key_mode"] == "caller":
        assert set(keys) == {case["idempotency_key"]}
    else:
        assert all(not key for key in keys)


def test_each_resource_has_success_and_typed_error() -> None:
    failing = False

    def handler(_: httpx.Request) -> httpx.Response:
        if failing:
            return httpx.Response(400, json={"error": {"code": "BAD_REQUEST", "message": "test failure"}})
        return httpx.Response(200, json=[] if current_path.endswith("/keys") else {})

    async def run() -> None:
        nonlocal failing, current_path
        client = NexoraClient("test-key", transport=httpx.MockTransport(handler))
        checks = [
            ("wallets", "/wallets", lambda: client.wallets.create()),
            ("transfers", "/transfers", lambda: client.transfers.create({})),
            ("fx", "/fx/rates", lambda: client.fx.get_rates({"from": "USD", "to": "USDC"})),
            ("schedules", "/schedules", lambda: client.schedules.list()),
            ("webhooks", "/webhooks", lambda: client.webhooks.list()),
            ("fees", "/fees", lambda: client.fees.get()),
            ("keys", "/keys", lambda: client.keys.list()),
            ("fiat", "/wallets/w-1/deposit/fiat", lambda: client.fiat.deposit("w-1", {})),
        ]
        try:
            for _, path, call in checks:
                current_path = path
                failing = False
                await call()
                failing = True
                with pytest.raises(ValidationError) as error:
                    await call()
                assert error.value.code == "BAD_REQUEST"
                assert error.value.http_status == 400
        finally:
            await client.aclose()

    current_path = ""
    asyncio.run(run())


def test_error_envelope_preserves_validation_details() -> None:
    async def run() -> None:
        client = NexoraClient(
            "test-key",
            transport=httpx.MockTransport(
                lambda _: httpx.Response(400, json={"error": {"code": "INVALID", "message": "bad input"}, "validation_errors": [{"field": "amount"}]})
            ),
        )
        try:
            with pytest.raises(ValidationError) as error:
                await client.transfers.create({})
            assert error.value.details == [{"field": "amount"}]
        finally:
            await client.aclose()

    asyncio.run(run())


def test_missing_api_key_is_rejected() -> None:
    with pytest.raises(ValueError, match="api_key"):
        NexoraClient("")