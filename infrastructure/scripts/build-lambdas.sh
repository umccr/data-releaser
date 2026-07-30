#!/usr/bin/env bash
#
# build-lambdas.sh — cross-compile the Go Lambda entrypoints into the CDK asset
# directories that lib/data-releaser-stack.ts references.
#
# The Authorization Service Lambdas (functions/authorization and functions/admin)
# are Go binaries deployed on the provided.al2023 runtime on arm64/Graviton. AWS
# custom runtimes require the executable to be named exactly `bootstrap`.
#
# This must be run before `cdk synth` or `cdk deploy` so the asset directories
# contain a real binary. The asset directories live under infrastructure/dist/
# and are git-ignored (regenerated on every build).
#
# Usage:
#   npm run build:lambdas
#   # or directly:
#   bash scripts/build-lambdas.sh
set -euo pipefail

# Resolve repository root: infrastructure/ is a top-level directory in the repo.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INFRA_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${INFRA_DIR}/.." && pwd)"

DIST_DIR="${INFRA_DIR}/dist"

# Go build target for the Lambda custom runtime (Req 16.6): linux/arm64, static.
export GOOS=linux
export GOARCH=arm64
export CGO_ENABLED=0

build_one() {
  local fn="$1"
  local out="${DIST_DIR}/${fn}"
  mkdir -p "${out}"
  echo "Building functions/${fn} -> ${out}/bootstrap (${GOOS}/${GOARCH})"
  ( cd "${REPO_ROOT}" && go build -trimpath -ldflags="-s -w" -o "${out}/bootstrap" "./functions/${fn}" )
}

build_one authorization
build_one admin

echo "Lambda bootstrap binaries built under ${DIST_DIR}"
