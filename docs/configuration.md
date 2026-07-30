# Configuration

The `data-releaser` CLI is configured with a YAML file. The file contains no
secrets — only a credential-source identifier and endpoint/behaviour settings.
The Data Control credentials themselves are provisioned externally and resolved
through the standard AWS credential chain (see
[`docs/usage.md`](usage.md#authentication)).

## Config file search paths

When `--config <path>` is **not** supplied, the tool searches these locations in
order and uses the first one that exists:

1. `./config.yaml` (the current working directory)
2. `~/.config/data-releaser/config.yaml`

If `--config <path>` **is** supplied, that exact file is used; if it does not
exist, the command fails immediately. If no config file can be resolved at all,
the command fails with a message listing the locations it looked in.

## Flag precedence

CLI flags override the corresponding config-file values. Currently one config
field is overridable by a global flag:

| Config field | Overriding flag | Effect |
| --- | --- | --- |
| `profile` | `--profile <name>` | The flag wins when set (non-empty). If the flag is omitted, the config-file `profile` is used; if that is also empty, the default AWS credential chain is used. |

Per-command inputs such as `--libraries`, `--account`, `--expiry`, and
`--duration` are command arguments, not config fields — they have no config-file
equivalent.

## Fields

Legend: **Required** fields must be present and non-empty or config validation
fails. **Optional** fields may be omitted; a default is applied where noted.

### Top level

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `profile` | optional | *(default AWS chain)* | Name of an AWS profile/credential source the SDK understands (SSO, `credential_process`, etc.). Omit to use the default credential chain. Overridable with `--profile`. |

### `warehouse` — OrcaVault Mart via Athena (Lake Formation share)

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `warehouse.athenaWorkgroup` | **required** | — | Athena workgroup in the Data Control (consumer) account. Results land in this workgroup's configured S3 output location. |
| `warehouse.database` | **required** | — | The Lake Formation resource-link database name (e.g. `mart`). |
| `warehouse.catalog` | optional | `AwsDataCatalog` | The Glue catalog (resource-link) to query. |

### `endpoints` — service endpoints

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `endpoints.authorization` | **required** | — | Base URL of the Authorization Service (used by `validate` reachability checks). |
| `endpoints.htsgetBaseUrl` | **required** | — | htsget-rs base URL embedded in generated manifests so researchers can build requests. |
| `endpoints.apiGatewayRestApiId` | optional | — | REST API ID of the Authorization Service API Gateway. Sourced from an Infrastructure_Stack output. Consumed by the `validate` API Gateway stage check, which reports the check as unavailable if it is unset. |
| `endpoints.apiGatewayStageName` | optional | `prod` | API Gateway stage name. Sourced from an Infrastructure_Stack output. Used together with the REST API ID by the `validate` API Gateway stage check. |

### `manifest` — Manifest_Bucket

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `manifest.bucket` | **required in practice** | — | The Data Control Manifest_Bucket name, sourced from an Infrastructure_Stack output. Never hard-code it. Config *validation* does not reject an empty value, but `share` fails preflight if it is empty, and `revoke`/`extend` skip the manifest update with a warning. |
| `manifest.keyTemplate` | optional | `manifests/{accountId}/manifest.json` | Per-account object key template. Only the `{accountId}` placeholder is supported; any other `{...}` token is rejected at config-validation time. |

Each researcher's manifest is written at the rendered key, e.g.
`manifests/534840902377/manifest.json`.

### `cache` — authorization response caching

These durations are Go duration strings (`1h`, `90m`, `60s`).

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `cache.ttlCeiling` | optional | `1h` | The `s-maxage` ceiling for allow responses (caps how long a decision may be reused; also the upper bound on revocation propagation). |
| `cache.denyTtl` | optional | `60s` | Negative-cache TTL for deny responses, so newly created shares become visible promptly. |

> These fields configure the CLI's view of the cache policy. The authoritative
> values used at request time come from the `Cache-Control` directives the
> deployed Authorization Service Lambda emits, capped by the TTL ceiling
> configured on the htsget-rs callout cache (see
> [`docs/deployment.md`](deployment.md)).

### `validation.canary` — optional canary dataset

Used only by `validate` (the Cross-Account S3 check and `--test-setup` mode).
The whole `validation.canary` block is optional; when absent, the canary-based
checks report themselves as skipped/unavailable.

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `validation.canary.libraryId` | optional | — | Library ID of a known canary dataset for the synthetic test share. |
| `validation.canary.account` | optional | — | Researcher account used for the synthetic canary share. |
| `validation.canary.bucket` | optional | — | Data Account bucket holding the canary object (for the cross-account HeadObject check). |
| `validation.canary.key` | optional | — | Object key of the canary object. |

## Annotated example

```yaml
# config.yaml — data-releaser configuration (no secrets live here)

# Single Data Control identity. Any profile the AWS SDK understands (SSO,
# credential_process, ...). Omit to use the default credential chain.
# Overridable at runtime with --profile.
profile: "data-control"

warehouse:                          # OrcaVault Mart via Athena (Lake Formation share)
  athenaWorkgroup: "primary"        # REQUIRED — workgroup in the Data Control (consumer) account
  database:        "mart"           # REQUIRED — Lake Formation resource-link database
  catalog:         "AwsDataCatalog" # optional — Glue catalog; defaults to AwsDataCatalog

endpoints:
  authorization:   "https://auth.data-control.umccr.org"           # REQUIRED
  htsgetBaseUrl:   "https://htsget-lattice.data-control.umccr.org"  # REQUIRED
  apiGatewayRestApiId: "abc123def4"  # from Infrastructure_Stack output; used by the validate stage check # pragma: allowlist secret
  apiGatewayStageName: "prod"        # from Infrastructure_Stack output; defaults to "prod"

manifest:                                        # Manifest_Bucket in Data Control
  bucket:      "data-releaser-manifests-916218008751"  # from Infrastructure_Stack output; never hard-code
  keyTemplate: "manifests/{accountId}/manifest.json"   # optional; only {accountId} is supported

cache:
  ttlCeiling: "1h"    # optional — s-maxage ceiling for allow responses (default 1h)
  denyTtl:    "60s"   # optional — negative cache for deny responses (default 60s)

validation:
  canary:                       # optional — used only by `validate`
    libraryId: "L2400999"
    account:   "534840902377"
    bucket:    "umccr-data-bucket"
    key:       "canary/known-object.bam"
```

## Mapping to Infrastructure_Stack outputs

Three config values are meant to be read from the deployed CDK stack outputs
rather than guessed. See [`docs/deployment.md`](deployment.md#stack-outputs) for
the full mapping:

| Config field | Stack output |
| --- | --- |
| `endpoints.apiGatewayRestApiId` | `ApiGatewayRestApiId` |
| `endpoints.apiGatewayStageName` | `ApiGatewayStageName` |
| `manifest.bucket` | `ManifestBucketName` |
