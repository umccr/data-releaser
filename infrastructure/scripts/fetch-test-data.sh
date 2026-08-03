#!/usr/bin/env bash
#
# fetch-test-data.sh — download the htsget-rs sample data into
# infrastructure/test-data/, laid out in the key structure DataShareTestStack
# uploads to the test bucket.
#
# Mirrors the dist/ Lambda-build pattern: the output is git-ignored and must
# exist before `cdk synth`/`cdk deploy` of DataShareTestStack. Run it with:
#
#   npm run fetch:test-data
#
# The source repo/ref are overridable for pinning:
#   HTSGET_RS_REPO=https://github.com/umccr/htsget-rs HTSGET_RS_REF=main \
#     npm run fetch:test-data
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INFRA_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
TEST_DATA_DIR="${INFRA_DIR}/test-data"

HTSGET_RS_REPO="${HTSGET_RS_REPO:-https://github.com/umccr/htsget-rs}"
HTSGET_RS_REF="${HTSGET_RS_REF:-main}"

# Source BAM in the htsget-rs repo -> destination key under the test bucket.
# The destination MUST match an entry in TEST_DATA_OBJECTS (lib/test-data.ts):
# the stack uploads whatever is under test-data/, and the mock Mart advertises
# these same keys, so a mismatch means htsget 404s on a "valid" share.
SRC_BAM="data/bam/htsnexus_test_NA12878.bam"
DEST_BAM="demo/L2400001/NA12878.bam"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

echo "Fetching htsget-rs sample data from ${HTSGET_RS_REPO}@${HTSGET_RS_REF}"
git clone --depth 1 --branch "${HTSGET_RS_REF}" --filter=blob:none --sparse \
  "${HTSGET_RS_REPO}" "${work}/htsget-rs"
git -C "${work}/htsget-rs" sparse-checkout set data/bam

src="${work}/htsget-rs/${SRC_BAM}"
if [[ ! -f "${src}" || ! -f "${src}.bai" ]]; then
  echo "error: expected ${SRC_BAM}(.bai) in the htsget-rs checkout" >&2
  exit 1
fi

dest="${TEST_DATA_DIR}/${DEST_BAM}"
mkdir -p "$(dirname "${dest}")"
cp "${src}"     "${dest}"
cp "${src}.bai" "${dest}.bai"

echo "Wrote:"
echo "  ${dest}"
echo "  ${dest}.bai"
