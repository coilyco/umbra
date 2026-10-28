#!/usr/bin/env bash
# Fail on any reachable known vulnerability. The tool version is pinned so a
# run is repeatable, while the vulnerability database it reads is always live.
set -euo pipefail

go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
