#!/usr/bin/env bash
#
# Compiles the Go Lambda functions under functions/ into assets/<name>/bootstrap,
# which DataReleaserStack deploys and the npm package ships.
#
# The binaries are static, stripped and carry no VCS stamp, so a function whose code
# has not changed builds to an identical asset and is not redeployed.

set -euo pipefail

infrastructure="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
root="$(dirname "$infrastructure")"

for name in authorization admin copycompletion; do
  out="$infrastructure/assets/$name"
  rm -rf "$out"
  mkdir -p "$out"
  (
    cd "$root"
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
      -trimpath -buildvcs=false -tags lambda.norpc -ldflags "-s -w" \
      -o "$out/bootstrap" "./functions/$name"
  )
done
