# backrestwatch chart

Install `oci://ghcr.io/zekihan/charts/backrestwatch` with an explicit complete
inventory and an explicit `accessControl.existingSecret` referring to an approved
existing monitoring credential. The chart refuses to render without it and never
issues a credential. The default `database/database/repo1` is illustrative. Replace it
before installation; an empty inventory is rejected.

```yaml
accessControl:
  existingSecret: backrestwatch-monitoring-auth
  secretKey: token
config:
  maxAgeSeconds: 28800
  maxSampleAgeSeconds: 180
  pollIntervalSeconds: 60
  requestTimeoutSeconds: 10
  collectionTimeoutSeconds: 45
  clusters:
    - namespace: database
      name: production
      repos:
        repo1: {}
        repo2: {maxAgeSeconds: 36000}
```

| values | default / behavior |
| --- | --- |
| `image.repository`, `tag`, `digest` | GHCR; tag defaults to appVersion; digest takes precedence |
| `accessControl.existingSecret`, `secretKey` | required existing Secret name; key defaults to `token`; read-only credential-file mount, no anonymous backup access |
| `config` | validated monitoring inventory and thresholds; bind address and state path are chart-managed |
| `resources` | 10m / 32Mi requests; 128Mi memory limit; no CPU limit |
| `service.port` | 8080; container port remains 8080 |
| `serviceAccount.create`, `name`, `annotations` | create a dedicated account; name required when creation disabled |
| `rbac.create` | list-only ClusterRole and binding on postgresclusters, jobs, cronjobs; names include a stable hash to avoid cross-namespace collisions |
| `persistence.existingClaim` | reuse a claim; otherwise create a retained 1Gi ReadWriteOnce PVC |
| `persistence.storageClass`, `size`, `annotations` | use default StorageClass when empty; default keep annotation |
| `podSecurityContext`, `securityContext` | non-root uid/gid 25100, fsGroup 25100, RuntimeDefault seccomp, read-only root filesystem, drop all capabilities |
| `probes.startup`, `liveness`, `readiness` | configurable probe objects, all default to `/healthz` |
| `networkPolicy.enabled` | false; enable only after setting API peers and monitoring clients |
| `networkPolicy.apiPeers`, `apiPorts` | explicit API Service/endpoint CIDRs; TCP 443 and 6443 |
| `networkPolicy.ingressFrom` | Kubernetes NetworkPolicy peers for monitoring clients |
| `networkPolicy.extraEgress` | optional additional rules, including DNS when using a custom API hostname |
| `podAnnotations`, `nodeSelector`, `tolerations`, `affinity` | standard pod placement and annotations |
| `imagePullSecrets`, `nameOverride`, `fullnameOverride` | standard chart settings |

The chart deliberately fixes replicas to one and uses `Recreate` so a single
durable checkpoint has one owner. Do not scale this deployment horizontally.
Use an existing claim to preserve history when changing a release name.

Cluster-wide `list` is required to detect unexpected clusters and repositories.
The role grants no writes, watches, Secret access, pods/exec, or backup operations.
Disable RBAC creation only when supplying equivalent permissions externally.
Chart templates do not install or modify PostgresClusters or backup resources.

With network policies enabled, provide API Service and real endpoint CIDRs for
your CNI's Service translation behavior. Ingress rules must allow your monitoring
client and any platform-specific probe traffic. Default API access uses an IP
address and needs no DNS rule. See `examples/network-policy.yaml` in the source
repository. The monitor stays ready when backup metadata is unhealthy, allowing
the caller to inspect 503 responses.

Keep the evidence PVC through upgrades. The root filesystem remains read-only;
only `/data` is writable. Backup completion metadata does not establish
restorability. See the application README for recovery and diagnostic behavior.

Backup routes require a bearer credential; `/healthz` remains minimal and public
for probes. Use approved TLS transport and monitoring clients. Secret projection
updates are read per request. A missing, short or malformed file fails closed.
The chart does not provision credentials, TLS or live trust policy.
This is a breaking endpoint change: existing monitoring clients, including Gatus,
must send `Authorization: Bearer <credential>`. Without the header they receive
401; a missing/invalid server credential file returns 503. Configure clients before
an approved rollout. See the application's breaking-change notice and verify
actual Gatus wiring separately; this PR does not change live monitoring.

For offline lint/render validation only, supply a synthetic reference such as
`--set accessControl.existingSecret=chart-validation-only`; this does not create a
Secret and does not constitute a deployable credential configuration.

Optional `extraContainers`, `extraVolumes`, `service.extraPorts`, and `networkPolicy.ingressPorts` allow an operator-managed TLS proxy. The operator must supply and validate its image, certificates, security context, ports, and network policy; the chart does not configure TLS automatically.
