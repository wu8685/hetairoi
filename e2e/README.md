# hetairoi e2e

End-to-end tests that drive hetairoi with the **official `anthropic` Python
SDK** (the same client real users point at the service), exercising everyday
agent use cases.

The suite is self-contained: it boots a **fake ahsir** (`e2e/fakeahsir`, a tiny
Go stand-in returning deterministic chat replies) and **hetairoi** — both via
`go run` — then runs the SDK against hetairoi. No real ahsir and no live LLM
required, so it is deterministic and CI-friendly.

## Scenarios

- `test_agents.py` — agent create / retrieve / **versioning** (update → v2, pin a
  version) / list; environment create.
- `test_sessions.py` — session create against an agent; **single-turn chat**
  (send `user.message`, stream events, read the reply); **multi-turn**; event
  list (history).

## Run

```sh
# prerequisites: go 1.23+, and the Python deps:
python3 -m pip install -r e2e/requirements.txt

./e2e/run.sh                 # self-contained (fake ahsir backend)
./e2e/run.sh -k session      # filter
```

### Against a real ahsir

Once ahsir supports inline agent registration and is running with a real
provider:

```sh
CMA_E2E_AHSIR_URL=http://127.0.0.1:9800 ./e2e/run.sh
```

The fake-ahsir fixture is skipped; hetairoi talks to your ahsir, and replies
come from the real LLM (adjust assertions that expect the fake's `Echo …` text).

## How it works

`conftest.py` provides session-scoped fixtures that launch both processes on free
ports, wait for readiness, and hand back an `anthropic.Anthropic` client with
`base_url` pointed at hetairoi (and `trust_env=False` to bypass the corporate
proxy for localhost). Logs land in `/tmp/cma-e2e-*.log`.
