"""Everyday agent-config lifecycle via the official SDK: create, retrieve,
version, list, plus an environment."""
import anthropic
import pytest


def test_agent_create_retrieve(client):
    a = client.beta.agents.create(
        name="researcher",
        model="claude-opus-4-8",
        system="You are helpful.",
    )
    assert a.type == "agent"
    assert a.id.startswith("agent_")
    assert a.version == 1
    assert a.name == "researcher"
    assert a.model.id == "claude-opus-4-8"
    assert a.system == "You are helpful."
    # required-list fields must be present (not null)
    assert a.tools == []
    assert a.skills == []
    assert a.mcp_servers == []

    got = client.beta.agents.retrieve(a.id)
    assert got.id == a.id
    assert got.version == 1


def test_agent_versioning(client):
    a = client.beta.agents.create(name="versioned", model="claude-opus-4-8", system="v1")
    a2 = client.beta.agents.update(
        a.id, version=a.version, model="claude-opus-4-8", name="versioned", system="v2"
    )
    assert a2.id == a.id
    assert a2.version == 2
    assert a2.system == "v2"

    latest = client.beta.agents.retrieve(a.id)
    assert latest.version == 2

    pinned = client.beta.agents.retrieve(a.id, version=1)
    assert pinned.version == 1
    assert pinned.system == "v1"


def test_agent_list(client):
    a = client.beta.agents.create(name="listed", model="claude-opus-4-8")
    ids = {x.id for x in client.beta.agents.list()}
    assert a.id in ids


def test_environment_create(client):
    e = client.beta.environments.create(name="default", config={"type": "cloud"})
    assert e.type == "environment"
    assert e.id.startswith("env_")
    assert e.name == "default"


def test_environment_update_archive_delete(client):
    e = client.beta.environments.create(name="env-crud", config={"type": "cloud"})

    u = client.beta.environments.update(e.id, name="env-renamed", metadata={"k": "v"})
    assert u.name == "env-renamed"
    assert u.metadata.get("k") == "v"

    a = client.beta.environments.archive(e.id)
    assert a.archived_at is not None

    d = client.beta.environments.delete(e.id)
    assert d.type == "environment_deleted"
    assert d.id == e.id

    with pytest.raises(anthropic.NotFoundError):
        client.beta.environments.retrieve(e.id)


def test_agent_archive(client):
    a = client.beta.agents.create(name="to-archive", model="claude-opus-4-8")
    archived = client.beta.agents.archive(a.id)
    assert archived.id == a.id
    assert archived.archived_at is not None
    # archived agents are excluded from list by default, included on request
    assert a.id not in {x.id for x in client.beta.agents.list()}
    assert a.id in {x.id for x in client.beta.agents.list(include_archived=True)}


def test_environment_list_excludes_archived(client):
    e = client.beta.environments.create(name="env-arch-list", config={"type": "cloud"})
    assert e.id in {x.id for x in client.beta.environments.list()}
    client.beta.environments.archive(e.id)
    assert e.id not in {x.id for x in client.beta.environments.list()}
    assert e.id in {x.id for x in client.beta.environments.list(include_archived=True)}
