# Event Bus — end-to-end example

An ops-alert auto-handling chain, top to bottom:

```
webhook (alert) ──► eventbus handler ──► cma-service session ──► ahsir agent ──► real shell commands
   POST /eventbus/events        Keyed: one session per subject (incident)        df / ls / cat ...
```

- **`cmd/eventbus-example`** runs cma-service with an event bus. At startup it
  seeds an `ops-responder` agent + an `ops` environment, and registers one
  handler (`ops-alerts`, **Keyed** by `subject`): every `alert` event routes to a
  session for that subject and prompts the agent to investigate.
- The agent is ahsir-backed and uses its shell tools to actually run commands.
- Related alerts for the same `subject` reuse one session (incident memory); the
  session stays addressable via the normal CMA API for a human to take over.

## Run

```sh
DEEPSEEK_API_KEY=sk-... ./run.sh
```

It boots a real ahsir scheduler + the example service, POSTs an alert, and prints
the session's event log as the agent works (tool_use / tool_result / agent.message).

Override the provider with `EXAMPLE_PROVIDER` / `EXAMPLE_API_KEY` / `EXAMPLE_MODEL`,
or point at an existing ahsir checkout with `AHSIR_DIR`.

## Send your own event

```sh
curl -X POST http://127.0.0.1:8790/eventbus/events -H 'content-type: application/json' \
  -d '{"id":"alert-2","type":"alert","subject":"high-cpu web-01","payload":{"cpu":"97%"}}'
```

The response carries the resolved `session_id`; read its turn via
`GET /v1/sessions/<id>/events`.
