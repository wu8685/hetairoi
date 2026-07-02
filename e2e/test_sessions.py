"""Everyday agent-usage flow via the official SDK: create a session against an
agent, send a user message, stream the events, and read the reply back."""
import time

import anthropic
import pytest


def _drive_turn(client, session_id, text, max_events=50, deadline_s=30):
    """Open the live event stream, then send one user message and collect events
    until the session goes idle. Stream-first matches the documented CMA pattern
    (the stream is a live tail; it does not replay prior turns).
    Returns (agent_texts, stop_reason_type)."""
    texts, stop = [], None
    end = time.time() + deadline_s
    with client.beta.sessions.events.stream(session_id=session_id) as stream:
        client.beta.sessions.events.send(
            session_id=session_id,
            events=[{"type": "user.message", "content": [{"type": "text", "text": text}]}],
        )
        for n, ev in enumerate(stream):
            if ev.type == "agent.message":
                texts.append("".join(b.text for b in ev.content))
            elif ev.type == "session.status_idle":
                stop = ev.stop_reason.type
                break
            elif ev.type == "session.status_terminated":
                break
            if n + 1 >= max_events or time.time() > end:
                break
    return texts, stop


def test_session_create(client, agent, environment):
    s = client.beta.sessions.create(
        agent=agent.id, environment_id=environment.id, title="chat"
    )
    assert s.type == "session"
    assert s.id.startswith("sesn_")
    assert s.status == "idle"
    assert s.agent.id == agent.id
    assert s.agent.version == agent.version
    assert s.environment_id == environment.id


def test_session_single_turn(client, agent, environment):
    s = client.beta.sessions.create(agent=agent.id, environment_id=environment.id)
    texts, stop = _drive_turn(client, s.id, "hello agent")
    assert stop == "end_turn"
    assert texts, "expected at least one agent.message"
    assert "hello agent" in texts[-1]  # fakeahsir echoes the prompt


def test_session_multi_turn(client, agent, environment):
    s = client.beta.sessions.create(agent=agent.id, environment_id=environment.id)
    t1, _ = _drive_turn(client, s.id, "first question")
    t2, _ = _drive_turn(client, s.id, "second question")
    assert "first question" in t1[-1]
    assert "second question" in t2[-1]

    # the full event log is retrievable (history replay surface)
    events = list(client.beta.sessions.events.list(session_id=s.id))
    kinds = [e.type for e in events]
    assert kinds.count("agent.message") >= 2
    assert "session.status_idle" in kinds


def test_session_archive_and_list_filters(client, agent, environment):
    s = client.beta.sessions.create(agent=agent.id, environment_id=environment.id)
    # listed, and filterable by agent_id
    assert s.id in {x.id for x in client.beta.sessions.list(agent_id=agent.id)}

    a = client.beta.sessions.archive(s.id)
    assert a.archived_at is not None
    assert a.status == "terminated"

    # excluded from the default list, included with include_archived
    assert s.id not in {x.id for x in client.beta.sessions.list()}
    assert s.id in {x.id for x in client.beta.sessions.list(include_archived=True)}


def test_session_delete_emits_event(client, agent, environment):
    s = client.beta.sessions.create(agent=agent.id, environment_id=environment.id)
    seen = []
    end = time.time() + 15
    with client.beta.sessions.events.stream(session_id=s.id) as stream:
        client.beta.sessions.delete(s.id)
        for ev in stream:
            seen.append(ev.type)
            if ev.type == "session.deleted":
                break
            if time.time() > end:
                break
    assert "session.deleted" in seen


def test_session_update_delete(client, agent, environment):
    s = client.beta.sessions.create(agent=agent.id, environment_id=environment.id)

    u = client.beta.sessions.update(s.id, title="renamed", metadata={"a": "b"})
    assert u.title == "renamed"
    assert u.metadata.get("a") == "b"

    d = client.beta.sessions.delete(s.id)
    assert d.type == "session_deleted"
    assert d.id == s.id

    with pytest.raises(anthropic.NotFoundError):
        client.beta.sessions.retrieve(s.id)


def test_session_observability_events(client, agent, environment):
    """A turn surfaces structured observability events (tool_use + model-request
    span) on the live stream, validated by the official SDK's event models."""
    s = client.beta.sessions.create(agent=agent.id, environment_id=environment.id)
    kinds = []
    tool_use = None
    tool_result = None
    with client.beta.sessions.events.stream(session_id=s.id) as stream:
        client.beta.sessions.events.send(
            session_id=s.id,
            events=[{"type": "user.message", "content": [{"type": "text", "text": "do something"}]}],
        )
        for ev in stream:
            kinds.append(ev.type)
            if ev.type == "agent.tool_use":
                tool_use = ev
            if ev.type == "agent.tool_result":
                tool_result = ev
            if ev.type == "session.status_idle":
                break
    assert "span.model_request_start" in kinds
    assert "span.model_request_end" in kinds
    assert "agent.tool_use" in kinds
    assert tool_use is not None
    assert tool_use.name == "Read"
    assert tool_use.input.get("path")  # input round-trips
    # the tool_result links back to its tool_use via id
    assert tool_result is not None
    assert tool_result.tool_use_id == tool_use.id


def test_session_interrupt(client, agent, environment):
    """Drive a slow turn, send user.interrupt mid-flight, and confirm the
    session settles back to idle (the turn was cancelled, not left hanging)."""
    s = client.beta.sessions.create(agent=agent.id, environment_id=environment.id)
    sent_interrupt = False
    saw_idle = False
    with client.beta.sessions.events.stream(session_id=s.id) as stream:
        client.beta.sessions.events.send(
            session_id=s.id,
            events=[{"type": "user.message", "content": [{"type": "text", "text": "__SLOW__ please wait"}]}],
        )
        for n, ev in enumerate(stream):
            if ev.type == "session.status_running" and not sent_interrupt:
                # Give the turn a moment to publish its cancelable task id, then
                # interrupt. fakeahsir announces the task id immediately.
                time.sleep(0.3)
                client.beta.sessions.events.send(
                    session_id=s.id, events=[{"type": "user.interrupt"}]
                )
                sent_interrupt = True
            elif ev.type == "session.status_idle":
                saw_idle = True
                break
            elif ev.type == "session.status_terminated":
                break
            if n + 1 >= 50:
                break
    assert sent_interrupt, "never sent the interrupt"
    assert saw_idle, "session did not return to idle after interrupt"


def test_session_events_pagination(client, agent, environment):
    s = client.beta.sessions.create(agent=agent.id, environment_id=environment.id)
    _drive_turn(client, s.id, "q1")
    _drive_turn(client, s.id, "q2")

    full = list(client.beta.sessions.events.list(session_id=s.id))
    assert len(full) > 2  # several status/message events per turn

    # auto-pagination with a small page size must yield the exact same sequence
    paged = list(client.beta.sessions.events.list(session_id=s.id, limit=2))
    assert [e.id for e in paged] == [e.id for e in full]

    # the first page exposes a forward cursor and is capped at the limit
    first = client.beta.sessions.events.list(session_id=s.id, limit=2)
    assert first.next_page is not None
    assert len(first.data) == 2

    # order=desc returns reverse-chronological, and paginates correctly too
    desc = list(client.beta.sessions.events.list(session_id=s.id, limit=2, order="desc"))
    assert [e.id for e in desc] == list(reversed([e.id for e in full]))
