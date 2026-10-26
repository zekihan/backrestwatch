# backrestwatch

Monitor Crunchy PostgreSQL / pgBackRest backup **completion metadata freshness**.
Backrestwatch reads Kubernetes objects, retains validated successful completions
across process restarts, and exposes JSON over HTTP. It performs no backup,
restore, database connection, Secret read, or notification operation.

| endpoint | response |
| --- | --- |
| `/healthz` | 200 while the HTTP process is serving |
| `/backups` | 200 when every expected repository is fresh and the inventory matches; otherwise 503 |
| `/backups/<namespace>/<cluster>/<repo>` | 200 for a fresh completion, 503 for unhealthy/unavailable evidence, 404 for an unknown repository |

Backup routes require `Authorization: Bearer <credential>` from an operator-managed
credential file. Unauthorized requests receive 401 without inventory or failure
details; an unavailable/invalid credential file fails closed with 503. `/healthz`
is deliberately unauthenticated and returns only process health.

Backup failures leave the Service and diagnostic endpoints reachable. All pod
probes use `/healthz`. Responses use `Cache-Control: no-store` and backup ages
continue increasing between samples.

## install

Requires the Crunchy `PostgresCluster` v1beta1 CRD, a working default StorageClass
(or an existing PVC), and cluster-wide read-only list permissions. Replace the
example inventory with every PostgresCluster and repository in your cluster.
Unexpected inventory fails the aggregate check, including clusters with no repos.

```sh
helm upgrade --install backrestwatch oci://ghcr.io/zekihan/charts/backrestwatch \
  --version 0.1.2 --namespace backup-monitoring --create-namespace \
  --values examples/values.yaml
kubectl -n backup-monitoring port-forward service/backrestwatch 8080:8080
curl -i -H "Authorization: Bearer $BACKRESTWATCH_MONITORING_TOKEN" http://localhost:8080/backups
```

Images are published as `docker.io/zekihan/backrestwatch:0.1.2` and
`ghcr.io/zekihan/backrestwatch:0.1.2` for Linux amd64 and arm64. The chart defaults
to GHCR and its `appVersion` image tag. Pin `image.digest` when required.
See the [chart reference](charts/backrestwatch/README.md) for configuration.

## completion evidence

Backrestwatch takes a complete, paginated sample of PostgresClusters, labelled
backup Jobs, and scheduled-backup CronJobs. It uses the newest eligible success:

- a Job with `succeeded > 0`, `Complete=True`, and a valid `completionTime`;
- a current PostgresCluster's successful `status.pgbackrest.scheduledBackups` entry;
- an eligible scheduled CronJob's `status.lastSuccessfulTime`.

Namespace, cluster and repository labels must match. CronJobs must have a
controller owner reference to the current PostgresCluster UID and API version.
Jobs with controller owners must resolve directly to the current cluster or
through an eligible current CronJob. Jobs without controller owners require
matching labels and creation within the current cluster's lifetime. All object
and completion timestamps must include a timezone and must not be in the future.
Old objects cannot supply a completion after a cluster has been recreated.

Repository identity includes the effective configured backend destination and path,
plus PVC binding identity where available. An observed destination change or
repository removal/reappearance discards the old success and persists a boundary,
even if no replacement backup exists yet. Only Jobs created strictly after that
boundary or newly created CronJobs can establish a new completion. Aggregate
PostgresCluster status and pre-change CronJobs cannot prove which destination a
run used, so they cannot restore health after a change; retain completed Jobs long
enough for at least one collection. An unchanged destination retains its valid
checkpoint across restarts, schedule edits, retention changes, and volume resizing.

A failed or running attempt cannot erase a validated successful completion.
Malformed eligible success timestamps fail that repository closed even when an older
valid checkpoint exists. Missing clusters/repositories, API collection failures,
pagination failures, and expired samples fail closed. Every repository's age is
evaluated against its current threshold on each HTTP request.

Completion freshness does **not** establish object-store readability, WAL
completeness, decryptability, or restorability. Perform isolated restore drills
separately. Metadata proves freshness only within observed destination changes:
initial collection without a checkpoint, unseen change/revert cycles, projected
Secret/ConfigMap changes, and configuration reconciliation races cannot establish
the storage actually used by a backup. No production backup or restore resources
are mutated by this service.

## durable state

The checkpoint is a small versioned JSON file at `/data/evidence.json`; no
database is needed. Each record carries the namespace/cluster/repo key, current
PostgresCluster UID, creation time, destination fingerprint, any observed-change
boundary, latest success (if available), and evidence source. Writes
use a temporary file, file sync, atomic rename, and directory sync before a sample
is published. A process lock prevents two writers from sharing the same file.

The chart runs one replica using `Recreate` and a retained ReadWriteOnce PVC.
Keep that PVC across upgrades and reinstallations; use `persistence.existingClaim`
to reuse it. Storage must support POSIX file locks, atomic rename, and sync.
Deleting the PVC discards evidence that Kubernetes may already have collected.
The service cannot recover an unobserved completion if every upstream success
source has disappeared before its first sample.

On restart, recovered evidence becomes usable only after a fresh API sample
confirms the current cluster UID, repository destination, and inventory. The checkpoint is never treated
as a fresh API sample. A corrupt or unsupported checkpoint remains untouched and
returns 503 while `/healthz` stays 200. Preserve the file, restore a known valid
copy or remove the corrupt file deliberately, then allow collection to retry.
Version 1 checkpoints are read for migration but have no destination identity;
the first successful sample writes version 2 and requires a new eligible backup.
This deliberately produces 503 until new evidence is observed. Older releases
cannot read version 2, so preserve an appropriate checkpoint when rolling back.
The process retries loading until recovery succeeds. A write failure also keeps
backup endpoints unhealthy; check volume permissions, capacity and storage health.
If a clock rollback puts a retained completion or safety boundary in the future,
the repository stays unhealthy while preserving that evidence across restarts.
Once the clock catches up, the usual freshness checks apply again.
Removing a repository from the configured inventory retains a safety record and
clears its saved success. Re-adding it requires eligible evidence after that
record's boundary; old aggregate status cannot restore health. These records
count toward the checkpoint's 4,000-repository limit. Exceeding the limit fails
closed without dropping records or replacing the checkpoint. Retiring old records
requires explicit operator approval and the [offline retirement runbook](docs/checkpoint-retirement.md),
including a validated backup, exact retired-key review, atomic replacement, fresh
API sample and rollback. Removal discards protection against stale evidence.
Observed destination changes and disappearances remain invalidated in memory
across failed writes, so storage recovery cannot resurrect the old completion.
New successes are retained only after a successful write. Pending invalidations
cannot survive a process exit until storage accepts a checkpoint; restore write
access before restarting whenever possible.

## configuration and operation

Use `--config /path/config.json`; see [the portable example](examples/config.json).
JSON rejects unknown fields, case-insensitive duplicate keys, trailing documents, duplicate
clusters, empty inventory, invalid names, and impossible timeout relationships.
Namespace and cluster names are lowercase DNS labels up to 63 characters; Crunchy
repositories are `repo1` through `repo4`. Configuration is loaded at startup;
Helm config changes trigger a rollout.

| setting | default | meaning |
| --- | --- | --- |
| `pollIntervalSeconds` | 60 | delay after each collection |
| `maxSampleAgeSeconds` | 180 | maximum age of a complete API sample |
| `maxAgeSeconds` | 28800 | default maximum successful completion age |
| `clusters[].repos.<repo>.maxAgeSeconds` | inherited | per-repository override; omitted or zero inherits |
| `requestTimeoutSeconds` | 10 | per-request deadline |
| `collectionTimeoutSeconds` | 45 | deadline for all lists, pages and retries |
| `listenAddress` | `:8080` | HTTP bind address |
| `backupsTokenFile` | `/var/run/backrestwatch-auth/token` | absolute path to operator-managed monitoring bearer credential |
| `stateFile` | `/data/evidence.json` | absolute checkpoint file path |

Sample age starts when collection begins. Backup age remains measured from the
successful completion time, without counting collection duration twice.

The poll interval plus two collection timeouts must be shorter than sample
lifetime, leaving room for the next collection to finish before the current
sample expires. Request timeouts must fit within collection timeout. The listen
port must be numeric and between 1 and 65535. All non-override intervals must
be positive. One list has a 100-page / 20,000-item budget and each page a 16 MiB
budget. Exceeding a budget rejects the whole sample. Repeated continuation tokens
or a changed list resource version also reject the sample.

In Kubernetes, the API origin comes from `KUBERNETES_SERVICE_HOST` and
`KUBERNETES_SERVICE_PORT`. Outside Kubernetes, provide `--api-url`, `--ca-file`,
and `--token-file`. HTTPS and CA verification are mandatory. Tokens are reread on
every request, including retries; redirects are rejected. Transient transport
errors, 401, 429 and 5xx responses retry up to three attempts with bounded,
jittered exponential backoff. Cancellation stops requests and polling. SIGINT
and SIGTERM drain HTTP with a fifteen-second shutdown deadline.

```sh
backrestwatch --config examples/config.json --check-config
backrestwatch --version
```

Logs are structured JSON. API diagnostics include the resource path and HTTP
status where available, without response bodies, tokens or authorization headers.
503 responses identify missing inventory, invalid metadata, stale completions,
stale samples, API failures, or persistence failures.

## Breaking change: backup endpoint authentication

Existing endpoint clients, including Gatus, must now send an approved credential
in `Authorization: Bearer <credential>` for both `/backups` and per-repository
routes. Anonymous requests that previously returned backup status now return 401;
a missing or invalid server credential file returns 503. `/healthz` remains 200
without a credential and cannot be used as a backup-freshness check.

Before an upgrade, arrange the existing Secret reference, configure each client's
header through its approved secret-management mechanism, and verify authenticated
200/503 backup responses and anonymous denial in an approved test environment.
Gatus wiring feasibility is separate acceptance work; no live Gatus configuration
or credentials are changed by this PR. Deploying this change before clients are
updated will break their backup checks.



The chart will not render until `accessControl.existingSecret` names an existing
Secret and `accessControl.secretKey` selects its credential key (default `token`).
It mounts that key read-only, never puts credential bytes in the ConfigMap, never
creates a Secret and grants no Kubernetes Secret-read API permission. The example
values name a placeholder Secret; an operator must supply an approved existing
credential before installation. This is an intentional secure installation change.

Credentials must contain 32–4096 visible ASCII bytes (a single final newline is
allowed within the 4096-byte file limit). The file is reread on every backup
request so projected Secret updates take effect without a restart. Clients must
send exactly one `Authorization: Bearer ...` header. Treat credentials as secrets;
use TLS through an approved authenticated ingress/service mesh or equivalent
protected transport, restrict permitted clients and avoid plaintext/shared logs.
The application does not terminate TLS. NetworkPolicy remains optional defense
in depth and needs operator-approved API peers and monitoring selectors.

Both aggregate and per-repository routes enforce the same boundary before
returning detailed status or unknown-repository information. `/healthz` remains
minimal and usable by platform probes independently of credentials. Local HTTP
fixtures verify denial, authorized detailed status, rotation and credential-file
failure. Actual unrelated-namespace denial, monitoring-identity access, TLS and CNI
probe behavior have not been exercised on a live cluster and require separate
operator approval. No live credentials or network settings were changed.

## development and releases

Requires Go 1.27.1, golangci-lint 2.14.0, Helm 4.3.0, Docker, GoReleaser 2.18.2,
and Python with `tests/helm/requirements.txt`.

```sh
make audit
make chart-package
make container-test
make release-check
```

Unit and race tests cover eligibility, overrides, inventory, pagination, token
rotation, failed collection, retained Jobs, recreated clusters, expired samples,
checkpoint recovery, HTTP routes and cancellation. `make test/integration` starts
the real binary against a disposable TLS Kubernetes fixture, restarts it using
the same checkpoint, rotates its token, injects API failures, changes cluster
identity, and checks graceful signal shutdown. It never reads the host kubeconfig.

Set `VERSION`, chart `version`, and chart `appVersion` to the same stable semantic
version. Commit after all checks pass, then tag `v<version>`. Release CI reruns
checks, publishes both image platforms to Docker Hub and GHCR, publishes the OCI
chart, verifies anonymous image manifests and an installable chart archive, and
creates a GitHub release with binaries, checksums and the verified chart. Only
release jobs receive publishing credentials; PR jobs use read-only permissions.
GoReleaser uses the validated tag as the binary version.

Run `./scripts/verify_release.sh 0.1.2` to repeat the public manifest and chart
checks. See [compatibility and provenance](docs/compatibility.md).

## licence

AGPL-3.0, following the standalone Hornkeeper convention. The original Apache-2.0
monitor contract and behavioral fixtures are attributed in [NOTICE](NOTICE), with
the source licence preserved in [LICENSES](LICENSES/Apache-2.0.txt).
