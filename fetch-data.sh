#!/usr/bin/env bash
#
# fetch-data — consumer-side demo helper for the Data Releaser htsget share.
#
# Runs on the DEMO-account side (the researcher's view). Given an htsget path it:
#   1. finds the in-VPC test Lambda in the HtsGetLatticeDemo stack,
#   2. invokes it (the Lambda calls htsget-rs over VPC Lattice),
#   3. pretty-prints the htsget response — an access error, or the data tickets,
#   4. on success, fetches every ticket (honouring its signed Range header) and
#      assembles them into a single file under ./demo-output/.
#
# The point is to keep the "other terminal" readable during a demo: you type a
# path, you see either a red DENIED block or a green ticket list, and a valid
# file appears on disk — no CDK-mangled function names, no hand-copying 2 KB
# presigned URLs, no remembering that the Range header is part of the signature.
#
# Usage:
#   ./fetch-data /reads/demo/L2400001/NA12878
#   ./fetch-data '/reads/demo/L2400001/NA12878?referenceName=1&start=0&end=100000'
#   ./fetch-data --profile demo /reads/demo/L2400002/NA12878
#
# Options:
#   --profile <name>   AWS profile for the demo account (or set AWS_PROFILE)
#   --region <name>    AWS region (default: ap-southeast-2)
#   --stack <name>     CloudFormation stack (default: HtsGetLatticeDemo)
#   -r, --refresh      Ignore the cached function name and re-resolve it
#   -v, --verbose      Print the resolved account and function name
#   -h, --help         Show this help and exit
#
# Env overrides: FETCH_DATA_FUNCTION, STACK_NAME, OUTPUT_DIR, AWS_PROFILE, AWS_REGION
#
# Exit codes: 0 success · 1 usage/setup · 2 test-Lambda error · 3 htsget denied · 4 ticket fetch failed

set -euo pipefail

STACK_NAME="${STACK_NAME:-HtsGetLatticeDemo}"
OUTPUT_DIR="${OUTPUT_DIR:-./demo-output}"
REFRESH=0
VERBOSE=0
REQ_PATH=""

usage() { sed -n '2,/^set -euo/p' "$0" | sed 's/^# \{0,1\}//; s/^set -euo.*//'; }

# ---- argument parsing ------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    --profile) export AWS_PROFILE="$2"; shift 2 ;;
    --region)  export AWS_REGION="$2"; shift 2 ;;
    --stack)   STACK_NAME="$2"; shift 2 ;;
    -r|--refresh) REFRESH=1; shift ;;
    -v|--verbose) VERBOSE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    --) shift; REQ_PATH="${1:-}"; break ;;
    -*) echo "unknown option: $1" >&2; exit 1 ;;
    *) REQ_PATH="$1"; shift ;;
  esac
done

export AWS_DEFAULT_REGION="${AWS_REGION:-${AWS_DEFAULT_REGION:-ap-southeast-2}}"

# ---- colours (disabled when not a TTY or NO_COLOR set) ---------------------
if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
  BOLD=$'\e[1m'; DIM=$'\e[2m'; RED=$'\e[31m'; GREEN=$'\e[32m'
  YELLOW=$'\e[33m'; CYAN=$'\e[36m'; RESET=$'\e[0m'
else
  BOLD=; DIM=; RED=; GREEN=; YELLOW=; CYAN=; RESET=
fi
hr() { printf '%s────────────────────────────────────────────────────────%s\n' "$DIM" "$RESET"; }
die() { printf '%serror:%s %s\n' "$RED" "$RESET" "$*" >&2; exit 1; }

# ---- dependencies ----------------------------------------------------------
for cmd in aws jq curl; do
  command -v "$cmd" >/dev/null 2>&1 || die "'$cmd' is required but not installed (try: sudo apt install $cmd)"
done

[[ -n "$REQ_PATH" ]] || { usage; exit 1; }
[[ "$REQ_PATH" == /* ]] || die "path must start with '/', e.g. /reads/demo/L2400001/NA12878"

# ---- resolve the test Lambda (cached; logical id survives name truncation) --
resolve_function() {
  aws cloudformation describe-stack-resources \
    --stack-name "$STACK_NAME" \
    --query "StackResources[?ResourceType=='AWS::Lambda::Function' && contains(LogicalResourceId, 'TestLambda')].PhysicalResourceId | [0]" \
    --output text 2>/dev/null
}

CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/fetch-data"
mkdir -p "$CACHE_DIR"
# Profile/stack names can contain '/' (e.g. SSO "acct/Role" names), which would
# turn the cache key into a nested path — flatten to a safe basename.
cache_base="${STACK_NAME}.${AWS_PROFILE:-default}"
CACHE_KEY="$CACHE_DIR/${cache_base//[!A-Za-z0-9._-]/_}"

if [[ -n "${FETCH_DATA_FUNCTION:-}" ]]; then
  FUNCTION="$FETCH_DATA_FUNCTION"
elif [[ $REFRESH -eq 0 && -s "$CACHE_KEY" ]]; then
  FUNCTION="$(cat "$CACHE_KEY")"
else
  FUNCTION="$(resolve_function)"
  [[ -n "$FUNCTION" && "$FUNCTION" != "None" ]] || die \
    "could not find the test Lambda in stack '$STACK_NAME'.
  Check you are using the demo-account profile (--profile) and that HtsGetLatticeDemo is deployed."
  printf '%s' "$FUNCTION" > "$CACHE_KEY"
fi

if [[ $VERBOSE -eq 1 ]]; then
  acct="$(aws sts get-caller-identity --query Account --output text 2>/dev/null || echo '?')"
  printf '%saccount%s %s   %sfunction%s %s\n' "$DIM" "$RESET" "$acct" "$DIM" "$RESET" "$FUNCTION"
fi

# ---- invoke ----------------------------------------------------------------
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
RESP="$TMP/response.json"; META="$TMP/meta.json"
PAYLOAD="$(jq -nc --arg p "$REQ_PATH" '{path:$p}')"

printf '\n%shtsget request%s  %s%s%s\n' "$BOLD" "$RESET" "$CYAN" "$REQ_PATH" "$RESET"

if ! aws lambda invoke --function-name "$FUNCTION" \
      --payload "$PAYLOAD" --cli-binary-format raw-in-base64-out \
      --output json "$RESP" > "$META" 2>"$TMP/err"; then
  printf '%serror:%s lambda invoke failed:\n' "$RED" "$RESET" >&2
  cat "$TMP/err" >&2
  exit 1
fi

fnerr="$(jq -r '.FunctionError // empty' "$META" 2>/dev/null || true)"
if [[ -n "$fnerr" ]]; then
  printf '%sLambda reported FunctionError=%s%s\n' "$RED" "$fnerr" "$RESET" >&2
  jq . "$RESP" >&2 || cat "$RESP" >&2
  exit 2
fi

# The test Lambda always returns {statusCode, body}, where body is a JSON STRING.
outer_body="$(jq -r '.body // empty' "$RESP")"
[[ -n "$outer_body" ]] || { printf '%serror:%s unexpected Lambda response:\n' "$RED" "$RESET" >&2; jq . "$RESP" >&2; exit 2; }

# Two shapes:
#   {error, message, stack}                          -> test Lambda itself failed
#   {message, latticeResponse:{statusCode, body,...}}-> it reached htsget-rs
if ! jq -e '.latticeResponse' >/dev/null 2>&1 <<<"$outer_body"; then
  printf '\n%s✖ test Lambda error%s (the request never reached htsget-rs)\n' "$RED" "$RESET"
  hr; jq -C '{error, message}' <<<"$outer_body"; hr
  if jq -e '.message == "Request timeout"' >/dev/null 2>&1 <<<"$outer_body"; then
    printf '%shint:%s the test Lambda has a very short socket timeout; retry once (warm) or patch it to 300000ms.\n' "$YELLOW" "$RESET"
  fi
  exit 2
fi

hts_status="$(jq -r '.latticeResponse.statusCode // "?"' <<<"$outer_body")"
hts_body="$(jq -c '.latticeResponse.body' <<<"$outer_body")"

# ---- denied / not found / any non-ticket response --------------------------
n_urls="$(jq -r '(.htsget.urls // []) | length' <<<"$hts_body" 2>/dev/null || echo 0)"
if ! [[ "$n_urls" =~ ^[0-9]+$ ]] || [[ "$n_urls" -eq 0 ]]; then
  printf '\n%shtsget %s no data returned%s\n' "$RED" "$hts_status" "$RESET"
  hr; jq -C '.' <<<"$hts_body" 2>/dev/null || printf '%s\n' "$hts_body"; hr
  exit 3
fi

# ---- success: tickets present ---------------------------------------------
fmt="$(jq -r '.htsget.format // "DATA"' <<<"$hts_body")"
printf '\n%shtsget %s%s %s%s%s ticket(s), %s%s%s\n' \
  "$GREEN" "$hts_status" "$RESET" "$BOLD" "$n_urls" "$RESET" "$BOLD" "$fmt" "$RESET"
hr
# Full htsget response — every ticket in full, no truncation.
jq -C '.' <<<"$hts_body"
hr

# ---- fetch every ticket in order and assemble ------------------------------
mkdir -p "$OUTPUT_DIR"
id="${REQ_PATH%%\?*}"; base="${id##*/}"; [[ -n "$base" ]] || base="download"
ext="$(printf '%s' "$fmt" | tr '[:upper:]' '[:lower:]')"
OUTFILE="$OUTPUT_DIR/${base}.${ext}"
: > "$OUTFILE"

printf '\n%sfetching %s ticket(s) → %s%s%s\n' "$BOLD" "$n_urls" "$CYAN" "$OUTFILE" "$RESET"

total=0
for ((i=0; i<n_urls; i++)); do
  url="$(jq -r ".htsget.urls[$i].url" <<<"$hts_body")"
  rng="$(jq -r ".htsget.urls[$i].headers.Range // \"\"" <<<"$hts_body")"
  part="$TMP/part.$i"

  if [[ "$url" == data:* ]]; then
    # Inline data: URI (htsget may return header blocks this way). Decode base64.
    b64="${url#*,}"
    if printf '%s' "$b64" | base64 -d > "$part" 2>/dev/null; then
      sz="$(wc -c < "$part")"; cat "$part" >> "$OUTFILE"; total=$((total+sz))
      printf '  %s[%d/%d]%s inline  %10s bytes  %s(data: URI)%s\n' "$GREEN" $((i+1)) "$n_urls" "$RESET" "$sz" "$DIM" "$RESET"
    else
      printf '  %s[%d/%d] failed to decode inline data%s\n' "$RED" $((i+1)) "$n_urls" "$RESET" >&2; exit 4
    fi
  else
    # Presigned S3 URL. The Range header is part of the signature — send exactly
    # the headers htsget returned, or S3 replies SignatureDoesNotMatch.
    hdr_args=()
    while IFS= read -r h; do [[ -n "$h" ]] && hdr_args+=(-H "$h"); done \
      < <(jq -r ".htsget.urls[$i].headers // {} | to_entries[] | \"\(.key): \(.value)\"" <<<"$hts_body")
    code="$(curl -sS -o "$part" -w '%{http_code}' "${hdr_args[@]}" "$url" 2>>"$TMP/curl.err" || echo 000)"
    sz="$(wc -c < "$part" 2>/dev/null || echo 0)"
    if [[ "$code" =~ ^2 ]]; then
      cat "$part" >> "$OUTFILE"; total=$((total+sz))
      printf '  %s[%d/%d]%s HTTP %s %10s bytes  %s%s%s\n' \
        "$GREEN" $((i+1)) "$n_urls" "$RESET" "$code" "$sz" "$DIM" "${rng:+range $rng}" "$RESET"
    else
      printf '  %s[%d/%d] HTTP %s FAILED%s\n' "$RED" $((i+1)) "$n_urls" "$code" "$RESET" >&2
      head -c 400 "$part" >&2 2>/dev/null || true; echo >&2
      exit 4
    fi
  fi
done

hr
printf '%sconcatenated%s %s%s%s  (%s bytes total)\n' "$BOLD" "$RESET" "$CYAN" "$OUTFILE" "$RESET" "$total"
