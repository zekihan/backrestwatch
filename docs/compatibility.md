# compatibility and provenance

The starting implementation is `backup_monitor.py`, including `Monitor`, and
`tests/test_monitor.py` from `zekihan/argocd` revision
`912c6a744a2dc9dde3bcf083c91d718ed18b3882`. The script's last source change is
`cd83be523` (`fix(crunchy-postgres-operator): retain backup freshness after job cleanup`).
The whole script, behavioral tests, chart defaults, RBAC, network policy,
probes, Gatus endpoint generation and Gatus policy were inspected before the port.

The HTTP routes, completion sources, freshness thresholds, unexpected-inventory
failures and process-only probes retain that contract. The Go service adds
durable checkpoints, validated configuration, bounded retries and cancellation,
graceful shutdown, updated age diagnostics, and stronger cluster-lifetime checks
for Jobs. Unknown repositories return 404 even before an initial sample or during
an API failure. Existing per-repository JSON fields remain available.

The unit fixtures port the Python scenarios: missing and overwritten status,
CronJob success after failed attempts, unrelated labels/owners, missing or invalid
times, status after Job collection, repository-specific grace periods, unexpected
inventory, failed/incomplete Jobs, pagination loops, API/sample failures, cached
age expiry, and HTTP routes. Additional cases cover persistent recovery, atomic
checkpoint reads, exclusive writers, cluster recreation with old Jobs finishing
late, CronJob owner chains, token replacement, page budgets, corruption, storage
failure, concurrent reports, and real process shutdown.

The client targets Kubernetes `batch/v1` and Crunchy
`postgres-operator.crunchydata.com/v1beta1` with scheduled-backup status fields
`repo`, `succeeded`, and `completionTime`. It does not claim compatibility with
arbitrary operators that merely reuse pgBackRest.

Primary references:

- [Kubernetes Job completion semantics](https://kubernetes.io/docs/concepts/workloads/controllers/job/)
- [CronJob API and lastSuccessfulTime](https://kubernetes.io/docs/reference/kubernetes-api/batch/cron-job-v1/)
- [Crunchy PostgresCluster API](https://access.crunchydata.com/documentation/postgres-operator/latest/references/crd/5.2.x/postgrescluster)

Project layout, lint selection, build flags, structured logging, non-root scratch
image, Conventional Commits, GoReleaser archives, OCI chart publishing and licence
follow the maintained `hornkeeper` and `git-mirrorer` repositories. Docker build
images use the exact digest already pinned by those siblings. Publishing secrets
are provisioned by the existing `terraform/git` repository module from its Doppler
environment. Application source and chart examples contain no homelab inventory,
addresses or operational credentials.

Repository identity also reads configured S3/GCS/Azure destinations, destination
options in `spec.backups.pgbackrest.global`, PVC binding settings, and
`status.pgbackrest.repos[].volume`. PGO itself resets repository initialization
when external configuration hashes or bound volumes change. Backrestwatch retains
a local change boundary because upstream Jobs and aggregate status do not all
carry the destination identity. After a detected change, aggregate scheduled
status is insufficient (even Job startTime can be reset on resume); a post-boundary
Job or newly created CronJob must establish the next success. See the README for
migration behavior and the remaining metadata-only observability limits.

## Breaking change: backup endpoint authentication

Endpoint callers, including existing Gatus checks, now need an operator-managed
file-backed bearer credential. Aggregate and per-repository routes require
`Authorization: Bearer <credential>`. Anonymous requests return 401; a missing or
invalid credential file returns 503. The chart requires an existing Secret
reference. `/healthz` remains public process health and does not establish backup
freshness. Configure clients and verify actual Gatus secret/header wiring before
an approved rollout; no live client or credential configuration is changed here.

Backup evidence evaluation and checkpoint source handling match the existing
implementation, including ownerless Job compatibility, current owner UID checks,
cluster lifetime checks and destination-change barriers. Labels and owner
references do not authenticate the producer; namespace write permissions and
trusted operator behavior remain part of the existing trust assumptions. This PR
adds no signing requirement and does not address producer forgery. No checkpoint
success is invalidated merely because of its evidence source.
