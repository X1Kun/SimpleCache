#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
# Explicit single-node development mode; exec lets signals reach the server.
export DISCOVERY_MODE=static
export SELF_ADDR=http://localhost:8001
export MEMBERS=http://localhost:8001
exec go run -buildvcs=false ./cmd/simplecache -port=8001 -api=true
