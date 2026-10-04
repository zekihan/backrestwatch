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
  --version 0.1.0 --namespace backup-monitoring --create-namespace \
  --values examples/values.yaml
kubectl -n backup-monitoring port-forward service/backrestwatch 8080:8080
curl -i http://localhost:8080/backups
```

Images are published as `docker.io/zekihan/backrestwatch:0.1.0` and
`ghcr.io/zekihan/backrestwatch:0.1.0` for Linux amd64 and arm64. The chart defaults
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

A failed or running attempt cannot erase a validated successful completion.
Malformed success timestamps fail that repository closed even when an older
valid checkpoint exists. Missing clusters/repositories, API collection failures,
pagination failures, and expired samples fail closed. Every repository's age is
evaluated against its current threshold on each HTTP request.

Completion freshness does **not** establish object-store readability, WAL
completeness, decryptability, or restorability. Perform isolated restore drills
separately. No production backup or restore resources are mutated by this service.

## durable state

The checkpoint is a small versioned JSON file at `/data/evidence.json`; no
database is needed. Each record carries the namespace/cluster/repo key, current
PostgresCluster UID, creation time, latest success, and evidence source. Writes
use a temporary file, file sync, atomic rename, and directory sync before a sample
is published. A process lock prevents two writers from sharing the same file.

The chart runs one replica using `Recreate` and a retained ReadWriteOnce PVC.
Keep that PVC across upgrades and reinstallations; use `persistence.existingClaim`
to reuse it. Storage must support POSIX file locks, atomic rename, and sync.
Deleting the PVC discards evidence that Kubernetes may already have collected.
The service cannot recover an unobserved completion if every upstream success
source has disappeared before its first sample.

On restart, recovered evidence becomes usable only after a fresh API sample
confirms the current cluster UID and inventory. The checkpoint is never treated
as a fresh API sample. A corrupt or unsupported checkpoint remains untouched and
returns 503 while `/healthz` stays 200. Preserve the file, restore a known valid
copy or remove the corrupt file deliberately, then allow collection to retry.
The process retries loading until recovery succeeds. A write failure also keeps
backup endpoints unhealthy; check volume permissions, capacity and storage health.

## configuration and operation

Use `--config /path/config.json`; see [the portable example](examples/config.json).
JSON rejects unknown fields, duplicate keys, trailing documents, duplicate
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
| `stateFile` | `/data/evidence.json` | absolute checkpoint file path |

Poll and collection intervals must be shorter than sample lifetime. Request
timeouts must fit within collection timeout. All non-override intervals must be
positive. One list has a 100-page / 20,000-item budget and each page a 16 MiB
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

Run `./scripts/verify_release.sh 0.1.0` to repeat the public manifest and chart
checks. See [compatibility and provenance](docs/compatibility.md).

## licence

AGPL-3.0, following the standalone Hornkeeper convention. The original Apache-2.0
monitor contract and behavioral fixtures are attributed in [NOTICE](NOTICE), with
the source licence preserved in [LICENSES](LICENSES/Apache-2.0.txt).
