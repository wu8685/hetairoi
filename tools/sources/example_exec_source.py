#!/usr/bin/env python3
"""Reference `exec` source plugin for hetairoi.

An `exec` source is any command hetairoi runs every `interval`; it must print a
JSON array of events to stdout. hetairoi fills `source`/`time`, dedups by `id`,
and routes each event to the matching handlers. See docs/EVENTBUS-SOURCES.md.

The contract (a public interface — versioned via HET_PROTOCOL):

    stdout: [{"id": str, "type": str?, "subject": str?, "payload": obj?}, ...]

- `id` is REQUIRED and is the dedup identity. Encode the item's *mutable version*
  into it (e.g. pr-<iid>-<sha>) so an unchanged item is deduped (no work) while a
  real change yields a new id (exactly one new turn). Entries without an id are
  skipped by hetairoi.
- `type` is optional; when omitted hetairoi uses the spec's `event_type`.
- hetairoi injects two env vars:
    HET_PROTOCOL  the contract version (currently "1"); gate your output on it.
    HET_SCRATCH   a per-source scratch dir (also the working dir). Persist a
                  cursor here for stateful sources; stateless ones need nothing.

Run it by hand to test — no hetairoi needed:

    python3 example_exec_source.py | jq
"""
import json
import os
import sys

PROTOCOL = "1"


def main() -> int:
    # Refuse to speak a protocol we weren't built for.
    got = os.environ.get("HET_PROTOCOL", PROTOCOL)
    if got != PROTOCOL:
        print(f"unsupported HET_PROTOCOL={got!r} (this plugin speaks {PROTOCOL})",
              file=sys.stderr)
        return 1

    # A stateless source: re-emit the current batch every tick and let the bus
    # dedup by id. Replace this block with a real upstream call (a CLI, an HTTP
    # API, ...). The id folds in a mutable version so a change re-fires exactly
    # once; here we use a fixed demo item.
    events = [
        {
            "id": "prcomment-3196-213601693",   # <iid>-<maxCommentId>: bump ⇒ new turn
            "type": "pr.comment",
            "subject": "3196",
            "payload": {
                "iid": 3196,
                "project": "org/repo",
                "comment_note": "please rebase onto main",
            },
        },
    ]
    json.dump(events, sys.stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main())
