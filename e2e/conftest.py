"""Pytest fixtures that boot fakeahsir + hetairoi and hand the official
Anthropic SDK back, pointed at hetairoi.

The whole suite is self-contained: it needs only `go` and the `anthropic` Python
SDK. No real ahsir and no live LLM — fakeahsir supplies deterministic replies so
assertions are stable.

Point at a REAL ahsir instead by exporting CMA_E2E_AHSIR_URL (and running ahsir
with a real provider); the fakeahsir fixture is then skipped.
"""
import os
import socket
import subprocess
import time
import urllib.request
from pathlib import Path

import httpx
import pytest
from anthropic import Anthropic

REPO_ROOT = Path(__file__).resolve().parents[1]


def _free_port() -> int:
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def _go_env(**extra) -> dict:
    env = dict(os.environ)
    env["GO111MODULE"] = "on"
    env.update(extra)
    return env


def _wait_http(url: str, timeout: float = 90.0):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=2) as r:  # noqa: S310
                if r.status < 500:
                    return
        except Exception as e:  # noqa: BLE001
            last = e
        time.sleep(0.4)
    raise RuntimeError(f"service at {url} not ready in {timeout}s (last={last})")


@pytest.fixture(scope="session")
def ahsir_url() -> str:
    """Boot fakeahsir (or use a real ahsir via CMA_E2E_AHSIR_URL)."""
    real = os.environ.get("CMA_E2E_AHSIR_URL")
    if real:
        yield real
        return

    port = _free_port()
    addr = f"127.0.0.1:{port}"
    log = open("/tmp/cma-e2e-fakeahsir.log", "w")
    proc = subprocess.Popen(
        ["go", "run", "./e2e/fakeahsir"],
        cwd=REPO_ROOT,
        env=_go_env(FAKEAHSIR_LISTEN=addr),
        stdout=log,
        stderr=subprocess.STDOUT,
    )
    try:
        _wait_http(f"http://{addr}/healthz")
        yield f"http://{addr}"
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
        log.close()


@pytest.fixture(scope="session")
def base_url(ahsir_url) -> str:
    """Boot hetairoi pointed at ahsir_url; return its base URL."""
    port = _free_port()
    state = "/tmp/cma-e2e-state.json"
    if os.path.exists(state):
        os.remove(state)
    log = open("/tmp/cma-e2e-hetairoi.log", "w")
    proc = subprocess.Popen(
        ["go", "run", "./cmd/hetairoi"],
        cwd=REPO_ROOT,
        env=_go_env(
            CMA_LISTEN=f"127.0.0.1:{port}",
            CMA_AHSIR_URL=ahsir_url,
            CMA_STATE_FILE=state,
        ),
        stdout=log,
        stderr=subprocess.STDOUT,
    )
    url = f"http://127.0.0.1:{port}"
    try:
        _wait_http(f"{url}/v1/agents")
        yield url
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
        log.close()


@pytest.fixture(scope="session")
def client(base_url) -> Anthropic:
    """Official Anthropic SDK, pointed at hetairoi. trust_env=False keeps the
    corporate proxy out of localhost calls."""
    return Anthropic(
        api_key="sk-cma-e2e",
        base_url=base_url,
        http_client=httpx.Client(trust_env=False, timeout=60.0),
    )


@pytest.fixture
def agent(client):
    return client.beta.agents.create(
        name="e2e-researcher",
        model="claude-opus-4-8",
        system="You are a concise research assistant.",
    )


@pytest.fixture
def environment(client):
    return client.beta.environments.create(name="e2e-default", config={"type": "cloud"})
