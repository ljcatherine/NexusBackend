#!/usr/bin/env bash
# Build the pinned DeepSeek Harness worker image for the orchestrator's `dsh` engine.
set -euo pipefail
repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
src="$repo_root/integrations/dsh"
version="$(cat "$src/version")"
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] || { echo 'Invalid DSH version' >&2; exit 1; }
grep -q "\"@deepseek-ai/dsh\": \"$version\"" "$src/package.json" || { echo 'package.json does not pin the DSH version' >&2; exit 1; }
mkdir -p "$repo_root/build"
context="$(mktemp -d "$repo_root/build/dsh-image.XXXXXX")"
trap 'rm -rf -- "$context"' EXIT
cp "$src/Dockerfile" "$src/package.json" "$src/package-lock.json" "$src/worker.mjs" "$src/mcp.mjs" "$context/"
docker build --build-arg "DSH_VERSION=$version" --tag nexus-dsh:local "$context"
docker image inspect --format '{{.Id}}' nexus-dsh:local
