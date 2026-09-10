#!/usr/bin/env bash
# Runs the model evaluation against the endpoint the agent is configured
# for (LLM_BASE_URL, LLM_MODEL and friends), passing any flags through.
# Deliberately not in CI: it needs a GPU endpoint and takes minutes.
set -euo pipefail
cd "$(dirname "$0")/.."
exec go run ./cmd/evalllm "$@"
