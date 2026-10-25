# Backup producer authentication

Jobs and CronJobs are writable metadata, even when their labels and controller
owner references exactly reproduce PGO objects. They cannot authenticate the
producer. Backrestwatch now ignores unsigned Job/CronJob successes and ownerless
Jobs. Configuring `evidencePublicKeys` enables Ed25519 verification; it does not
establish a trusted issuer by itself. No keys or credentials are created by the
application or chart.

The default is **protected PostgresCluster status only**. This assumes Kubernetes
RBAC and the status subresource reserve PostgresCluster status writes to trusted
PGO identities, and that PGO's status is derived from authentic backup activity.
Operators must verify this assumption, including whether their operator can be
induced to publish forged Job success. If it does not hold, do not use status-only
monitoring as evidence of authentic backup completion. The application cannot
identify an API object's creating principal from owner references.

Aggregate status does not identify the destination. It remains ineligible after
an observed destination change or disappearance. Without a trusted attestation
issuer, such a repository deliberately remains unhealthy. A status-only default
must not be interpreted as complete compatibility with every PGO version or
cleanup policy.

## Optional issuer contract

Configure at most 16 `evidencePublicKeys` entries, mapping a DNS-label key ID to a
base64-encoded 32-byte Ed25519 public key. These are public verification keys;
private keys must never be accessible to ordinary Job/CronJob creators or to
backrestwatch. Use an independently protected controller/verifier that verifies
the actual backup and its destination **before** signing. A webhook that merely
signs caller-supplied labels, owner references or successful Job status recreates
the vulnerability. Issuer design, key provisioning and admission/RBAC changes
require separate operator decisions and are not implemented or deployed here.

After verification, the issuer attaches:

- `backrestwatch.io/attestation-key`: the configured key ID;
- `backrestwatch.io/attestation-signature`: standard padded base64 of the 64-byte
  Ed25519 signature.

The signed bytes are the compact UTF-8 JSON encoding of this ordered string array,
with the Go `encoding/json` escaping rules and no trailing newline:

```text
["backrestwatch-evidence/v1", kind, namespace, name, objectUID,
 creationTimestamp, clusterUID, clusterCreationTimestamp, repo,
 repositoryIdentity, completionTimestamp]
```

`kind` is exactly `Job` or `CronJob`. Timestamps use the exact API strings.
`completionTimestamp` is Job `status.completionTime` or CronJob
`status.lastSuccessfulTime`. `repositoryIdentity` is the lowercase hex SHA-256
computed by `internal/monitor/repository.go`, including the effective destination
and PVC binding. `monitor.AttestationMessage` is the reference encoder. Signatures
bind a completion to the object UID, current cluster lifetime, repository and
destination. Copying an annotation to another object or changing a bound field
fails verification. Existing completion/ownership/time/change checks still apply.

The durable source includes a SHA-256 public-key fingerprint. Cached Job/CronJob
successes remain usable after object collection only while that key remains in
the configured trust set. Key removal or replacement invalidates those successes
and advances a persistent safety boundary on the next sample. Renaming a key ID
without changing its public key preserves cached evidence. Configuration is loaded
at startup; a trust-set change needs an approved rollout.

## Upgrade and acceptance

Legacy `Completed Job` and `CronJob lastSuccessfulTime` checkpoint successes are
unsigned. They are cleared with a new boundary, even if their old objects are
still present. A new authentically signed backup object created after that
boundary is required. Protected status checkpoints retain their previous freshness
behavior. Review this intentional compatibility change before upgrading; do not
delete checkpoint history simply to bypass it. Older binaries do not enforce the
new source policy and must not be used as a security rollback without review.

Local fixtures reject ownerless successes, exact forged PostgresCluster and
CronJob owner chains, unknown signatures, copied/replayed signatures and revoked
keys. Signed synthetic PGO-shaped Jobs and CronJobs remain accepted, and existing
stale-object, future-time, recreation and destination-change regressions still
run. The real process fixture verifies signed Job recovery after a destination
change and restart.

Not run against a real cluster: create under an untrusted identity, authentic
issuer end-to-end verification, RBAC/status protection, admission behavior or PGO
version compatibility. These require explicit operator approval and isolated
acceptance resources; no live trust policy was changed for this implementation.
