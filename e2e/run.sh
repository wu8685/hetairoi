#!/usr/bin/env bash
# Run the cma-service e2e suite. Boots fakeahsir + cma-service (via `go run`) and
# drives them with the official Anthropic SDK. Needs `go` and Python deps
# (see requirements.txt).
#
#   ./e2e/run.sh                 # self-contained (fakeahsir backend)
#   CMA_E2E_AHSIR_URL=http://127.0.0.1:9800 ./e2e/run.sh   # against a real ahsir
set -euo pipefail
cd "$(dirname "$0")/.."

python3 -m pytest e2e -v "$@"
