# Operations

This runbook covers local development and the Phase 9 production deployment
contract. Read [architecture.md](architecture.md) for dependency and consistency
boundaries, [security.md](security.md) before changing trust, secrets, or network
policy, [release.md](release.md) for trusted publication and promotion, and
[backup-restore.md](backup-restore.md) for recovery and credential rotation.

The Kubernetes manifests deploy only user-service, order-service,
payment-service, job-worker, and the migrator Job. They do not deploy
PostgreSQL, queue Redis, optional cache Redis, an OpenTelemetry Collector,
Prometheus storage, an ingress/Gateway, or certificate management. Those
dependencies must exist and be operated separately.

## Repository contracts

- Build and validation targets: [`Makefile`](../Makefile)
- OCI image definition: [`Containerfile`](../Containerfile)
- HTTP contract: [`api/openapi/commerce.yaml`](../api/openapi/commerce.yaml)
- Kubernetes base: [`deploy/kubernetes/base`](../deploy/kubernetes/base/)
- Production overlay: [`deploy/kubernetes/overlays/production`](../deploy/kubernetes/overlays/production/)
- Production placeholders and prerequisites: [overlay README](../deploy/kubernetes/overlays/production/README.md)
- Required externally managed Secret keys: [Secret contract](../deploy/kubernetes/overlays/production/secrets-contract.md)
- CI gates and produced artifacts: [`.github/workflows/ci.yml`](../.github/workflows/ci.yml)
- Trusted tag publication, release evidence, admission, and promotion: [release.md](release.md)
- External backup, restore, and credential-rotation contract: [backup-restore.md](backup-restore.md)

## Local operation

Local infrastructure uses rootless Podman Quadlet and systemd user services.
It is separate from the production Kubernetes model.

Prerequisites are Go 1.26.6 or newer, Podman with Quadlet support, a working
systemd user session, PostgreSQL client tools, and OpenSSL. Initialize local
configuration and owner-only secrets once, then start PostgreSQL and Redis:

```bash
make init
make infra-up
make migrate-up
```

Run each resident process in its own terminal, starting user-service first:

```bash
make run-user
make run-order
make run-payment
make run-worker
```

`make run` is an alias for `make run-user`. The targets load `.env`, inject
files from `.secrets/`, keep plaintext listeners on loopback, and map the four
metrics ports. Check local state with:

```bash
make infra-status
make migrate-version
curl --fail http://127.0.0.1:8081/healthz
curl --fail http://127.0.0.1:8082/readyz
curl --fail http://127.0.0.1:8083/readyz
curl --fail http://127.0.0.1:9104/metrics
```

Use `make infra-logs` for PostgreSQL and Redis journal output. Stop local
infrastructure with `make infra-down`. Do not point `DATABASE_URL`,
`TEST_DATABASE_URL`, or `TEST_REDIS_DB` at data that must be retained; the
integration target recreates the marked test database and flushes its Redis DB.

Before a release, run the same gates referenced by CI:

```bash
make check
make release-check
```

`make release-check` includes destructive isolated integration tests and
reachable-vulnerability scanning, so it requires a correctly initialized local
test environment. Focused contract checks are available as
`make openapi-check`, `make kube-check`, and `make workflow-check`.
`make benchmark` is informational; its CI output is not a production capacity
guarantee.

## Image construction

Build one of the six production binaries into an OCI image:

```bash
make image \
  SERVICE=user-service \
  IMAGE_PREFIX=registry.example.com/distributed-commerce \
  IMAGE_VERSION="$GIT_SHA" \
  IMAGE_REVISION="$GIT_SHA"
```

Valid `SERVICE` values are `user-service`, `order-service`, `payment-service`,
`job-worker`, `job-admin`, and `migrator`. Build all six with:

```bash
make images IMAGE_PREFIX=registry.example.com/distributed-commerce IMAGE_VERSION="$GIT_SHA"
```

The final image is a scratch image containing the selected `/app/service`, the
`/app/loopback-probe` helper, and the public CA bundle. It runs as
`65532:65532`. The CI image job builds and archives each image and verifies the
configured user. Separately, the Phase 9 trusted tag workflow publishes all six
images to GHCR, scans them, generates SPDX SBOMs, signs their digests, publishes
GitHub provenance and SBOM attestations, and records release evidence. Cluster
admission and promotion are separate controls described in
[release.md](release.md); the repository does not install or operate them.

Use the approved GHCR digests, then replace every `registry.example.invalid`
name and all-zero digest in the
[production kustomization](../deploy/kubernetes/overlays/production/kustomization.yaml).
Deploy immutable digests, not mutable tags, and follow the
[overlay promotion procedure](release.md#overlay-promotion).

## Production prerequisites

### External dependencies

Provision and validate these before creating application Pods:

1. A PostgreSQL service reachable by all application roles, with external full
   backups and PITR meeting the
   [recovery contract](backup-restore.md#postgresql-backup-policy). It must
   support the `pgcrypto` extension and verified TLS. Use a schema-owner URL for
   the migrator and least-privilege, process-specific runtime URLs.
2. A dedicated queue Redis service with persistence, bounded memory,
   `noeviction`, an ACL user, and verified TLS. Queue durability and restore are
   external operational responsibilities.
3. Optionally, a separate cache Redis service with an eviction policy suitable
   for cache data. The base intentionally leaves `CACHE_REDIS_ADDR` unset.
4. Optionally, an external OpenTelemetry Collector and metrics backend. Neither
   is present in the manifests.
5. A trusted ingress or Gateway implementation and DNS. The repository does not
   contain an Ingress or Gateway resource.

Replace the external PostgreSQL and queue Redis placeholder hostnames in the
deployment configuration. Add environment-specific egress controls using real
CIDRs or an FQDN-aware CNI; the base NetworkPolicies restrict ingress only.

### Secrets and certificates

Create all Secrets listed in the [Secret contract](../deploy/kubernetes/overlays/production/secrets-contract.md)
through an external secret controller or audited deployment system. The
rendered Kustomize output intentionally contains no `Secret` resource. At a
minimum, verify:

- Every database URL uses `sslmode=verify-full`, names the expected database
  host, and uses `/var/run/secrets/postgres/ca.crt` as its trust root.
- `commerce-runtime` has five database URL keys intended for distinct database
  roles, the 64-character hex PASETO key, and the queue Redis password. Only
  user-service references the PASETO key.
- The three HTTP certificates cover the DNS names used by the ingress or
  Gateway. The edge must use TLS passthrough or verify the native HTTPS
  upstream; do not silently downgrade the upstream hop.
- The user gRPC server certificate covers
  `commerce-user.commerce.svc.cluster.local` as a DNS SAN.
- The order and payment gRPC client certificates chain to
  `commerce-identity-ca` and each contain exactly one URI SAN:
  `spiffe://commerce.internal/order-service` or
  `spiffe://commerce.internal/payment-service`, respectively.
- The queue Redis CA validates the configured Redis server name. If the Redis
  provider requires client certificates, extend the manifests and Secret
  contract before rollout; the base supplies password authentication only.
- Secret files are present, readable by UID/GID 65532, unexpired, and not
  written into ConfigMaps, rendered manifests, logs, or CI artifacts.

Label only the trusted ingress controller namespace with
`commerce.network/ingress=true`. Without that label, the default-deny ingress
policy blocks API traffic. Review the exact rendered resources with:

```bash
make kube-check
bin/kustomize-v5.8.1 build deploy/kubernetes/overlays/production
```

Treat the second command's output as sensitive operational metadata even
though it must not contain secret values.

## Migration-first rollout

Kustomize renders resources but does not order a Job before Deployments. A
single `kubectl apply -k` is therefore not migration-first. The delivery system
must partition the rendered objects or use equivalent synchronization.

1. Pass `make release-check`, complete the
   [trusted release gates](release.md#verification-and-evidence), approve
   immutable image digests, and confirm an external database recovery point.
   Review every migration under
   [`db/migrations`](../db/migrations/) for forward and backward binary
   compatibility.
2. Render and validate the production overlay with `make kube-check`. Reject
   placeholder registries, zero digests, placeholder dependency hosts, and any
   rendered Secret data.
3. Apply the namespace, service accounts, ConfigMaps, Services, PDBs, and
   NetworkPolicies, but keep new Deployments from starting. Ensure externally
   managed Secrets and certificate mounts are already reconciled.
4. Apply a fresh `commerce-migrate` Job from that same rendered release. The Job
   template is immutable; if a prior completed Job with that fixed name still
   exists, retain its logs, then delete it before applying the new template, or
   have the delivery system give each release a unique Job name.
5. Wait for completion and inspect its output:

```bash
kubectl -n commerce wait --for=condition=complete job/commerce-migrate --timeout=330s
kubectl -n commerce logs job/commerce-migrate
```

The Job has a 300-second active deadline, a backoff limit of three, and applies
embedded migrations upward only. Do not continue if it fails, times out, or
reports a dirty schema.

6. Apply or scale up user-service first. Then apply order-service,
   payment-service, and job-worker. Wait for each rollout:

```bash
kubectl -n commerce rollout status deployment/commerce-user --timeout=5m
kubectl -n commerce rollout status deployment/commerce-order --timeout=5m
kubectl -n commerce rollout status deployment/commerce-payment --timeout=5m
kubectl -n commerce rollout status deployment/commerce-worker --timeout=5m
```

7. Confirm all API readiness responses, identity-backed authentication, one
   idempotent order replay, one idempotent payment replay in a controlled test
   account, and worker queue health before normal traffic. Do not infer
   dependency readiness from liveness alone.

The base deployment uses two replicas and `maxUnavailable: 0`, `maxSurge: 1`
for each API. The worker is a single replica with `Recreate`; expect a worker
processing gap during its rollout.

## Probe contract

| Workload | Startup | Readiness | Liveness |
| --- | --- | --- | --- |
| User API | HTTPS `/healthz`, up to about 60 seconds | HTTPS `/readyz` every 5 seconds; PostgreSQL plus local identity listener state | HTTPS `/healthz` every 10 seconds |
| Order API | HTTPS `/healthz`, up to about 60 seconds | HTTPS `/readyz`; PostgreSQL schema plus identity gRPC health | HTTPS `/healthz` every 10 seconds |
| Payment API | HTTPS `/healthz`, up to about 60 seconds | HTTPS `/readyz`; PostgreSQL payment invariants plus identity gRPC health | HTTPS `/healthz` every 10 seconds |
| Worker | Exec TCP check of `127.0.0.1:9104`, up to about 60 seconds | The same loopback TCP check | The same loopback TCP check |

API liveness is intentionally independent of dependencies. Kubernetes HTTPS
probes test the endpoint but do not validate its certificate chain or hostname;
certificate validation belongs in rollout and synthetic checks. User readiness
does not include queue Redis, and order readiness does not include cache Redis.

The worker opens and pings PostgreSQL and queue Redis before starting its
metrics listener, so startup catches initial dependency failure. After startup,
its probes prove only that the process-owned loopback listener accepts TCP.
Monitor Asynq health logs and task metrics for later database or Redis failure.

## Metrics loopback

| Process | Address |
| --- | --- |
| user-service | `127.0.0.1:9101/metrics` |
| order-service | `127.0.0.1:9102/metrics` |
| payment-service | `127.0.0.1:9103/metrics` |
| job-worker | `127.0.0.1:9104/metrics` |

These ports are not declared by a Kubernetes Service and cannot be scraped by
a conventional cross-Pod `ServiceMonitor`. Run a trusted same-Pod scraper or
proxy sidecar that forwards to a protected telemetry backend. A bind failure is
a process failure, not a loss of optional monitoring.

For a controlled diagnostic, port-forward one Pod and query locally:

```bash
kubectl -n commerce port-forward pod/<user-pod> 9101:9101
curl --fail http://127.0.0.1:9101/metrics
```

The application containers are scratch images with no shell or `curl`.
`/app/loopback-probe` can only verify that a loopback TCP port accepts a
connection; it does not parse Prometheus output.

## OTLP trace export

The base sets `OTEL_TRACES_EXPORTER=none`. To enable export, extend every
resident workload with:

- `OTEL_TRACES_EXPORTER=otlp`.
- An HTTPS root OTLP/gRPC endpoint with no URL path.
- A read-only projected CA Secret and
  `OTEL_EXPORTER_OTLP_CERTIFICATE`, unless system roots are intentionally used.
- Optional paired client certificate/key paths for mTLS.
- Any `OTEL_EXPORTER_OTLP_HEADERS` from a Secret-backed environment value, not
  a ConfigMap.
- An explicit starting sample ratio, normally the existing `0.1`, reviewed
  against measured volume and privacy requirements.

Apply egress policy for the real Collector and roll the Pods because telemetry
configuration and certificates load only at startup. Export uses a bounded
batch queue and is not part of readiness. Collector outage can cause trace loss
without stopping requests. The Collector, storage, dashboards, SLOs, and alerts
remain external deployment concerns.

## Capacity budget

The base is a starting reservation, not measured sizing:

| Workload | Replicas | Request per Pod | Limit per Pod | `GOMEMLIMIT` |
| --- | ---: | --- | --- | --- |
| User | 2 | `250m`, `256Mi` | `2`, `512Mi` | `400MiB` |
| Order | 2 | `100m`, `128Mi` | `1`, `256Mi` | `192MiB` |
| Payment | 2 | `100m`, `128Mi` | `1`, `256Mi` | `192MiB` |
| Worker | 1 | `100m`, `128Mi` | `1`, `256Mi` | `192MiB` |
| Migrator, transient | 1 | `50m`, `64Mi` | `500m`, `128Mi` | unset |

Steady resident requests total `1000m` CPU and `1152Mi` memory; limits total 9
CPU and `2304Mi`. If all three API Deployments surge by one Pod together, the
resident peak is `1450m` and `1664Mi` requested, with limits of 13 CPU and
`3328Mi`. Accidental overlap with the migrator raises that to `1500m`,
`1728Mi`, 13.5 CPU, and `3456Mi`. Reserve node and zone headroom for system
Pods, the ingress, telemetry sidecars, and disruption; these totals do not
include external dependencies.

At the configured `DB_MAX_CONNS`, resident application pools can consume up to
65 PostgreSQL connections: 20 each for user, order, and payment replicas plus 5
for the worker. Simultaneous API surge can raise this to 95, excluding the
migrator, operator sessions, and database maintenance. Size the database or
pooler with explicit headroom before scaling replicas.

Queue Redis pool sizes default to 10 per client, giving up to 30 configured pool
slots across two user Pods and one worker, plus an on-demand admin client. Cache
Redis adds 10 per order Pod when enabled. Recalculate both database and Redis
budgets for every replica or pool-size change.

Each user Pod permits two concurrent 64 MiB Argon2id operations by default.
Load-test registration and login separately from cheap routes, watch throttling
and memory, and do not derive production throughput from the informational CI
benchmarks. There is no HPA in the repository. PDBs protect one available API
replica only during voluntary disruption; they do not protect the single
worker or an external dependency.

## Rollback

Application rollback and schema rollback are separate decisions.
Release evidence and digest rollback are detailed in
[release.md](release.md#failure-and-rollback); database recovery and dirty
migration handling are detailed in
[backup-restore.md](backup-restore.md#migration-dirty-state).

1. Stop the rollout and preserve logs, events, the migration result, image
   digests, and request IDs that demonstrate the failure.
2. If the new schema is backward-compatible with the previous binary, restore
   the previous immutable image digests in the overlay and reconcile. A manual
   `kubectl rollout undo` is temporary if GitOps still declares the new digest.
3. Keep user-service compatible with the deployed order/payment clients. A
   protected request requires the identity RPC throughout rollback.
4. Do not run `make migrate-down` as a routine production rollback. That target
   uses the local CLI contract and reverts one migration; down migrations can
   destroy data, and the Kubernetes migrator supports only `up`.
5. For an incompatible or failed migration, stop writes, assess the dirty
   version with the database operator, and prefer a reviewed forward repair.
   Restore a tested backup only under the external database recovery plan.
6. Disable optional traces with `OTEL_TRACES_EXPORTER=none` and roll out if the
   exporter is the issue. The production base already permits cache rollback by
   leaving it disabled.

Never roll back only one side of a database contract without proving that the
old binary accepts the current schema. Preserve idempotency records during
recovery; deleting them can make an old mutation executable again.

## Task recovery

Local archived-task operations are deliberately single-item:

```bash
make jobs-failed
make jobs-failed PAGE=2
make jobs-retry ID=<task-id>
make jobs-delete ID=<task-id>
```

Each list page contains at most 100 task metadata records and omits payloads.
Classify `last_error` before mutation. Fix a transient PostgreSQL, Redis, TLS,
or schema problem before retrying. Invalid task types or payloads are permanent
errors and should normally be retained for investigation, then deleted under
an audit trail rather than retried repeatedly.

The Kubernetes base does not deploy `job-admin`. In production, run its approved
image as a short-lived, access-controlled workload with only queue Redis
address, queue name, password, CA, and server-name settings. Do not give it the
PASETO key or database credentials. Capture operator identity and the selected
task ID. There is no bulk wildcard operation.

Asynq delivery is at least once. A retry may run work that completed before its
acknowledgement, which is safe only because the current cleanup handler is
idempotent. Redis persistence, backup, failover, and queue restoration are not
implemented by these manifests; follow the external
[queue recovery contract](backup-restore.md#queue-redis-persistence-and-recovery).
Never flush or repurpose the production queue database to recover application
availability.

## Certificate and key rotation

HTTP, gRPC, Redis, PostgreSQL, OTLP certificates, CA pools, and the PASETO key
are loaded at process startup. Projected Secret file updates do not reload them;
every change requires a controlled rollout.
Database and Redis credential rotation, dangerous-operation approval, and the
complete recovery boundary are in
[backup-restore.md](backup-restore.md#credential-rotation).

For a leaf certificate under an unchanged CA, publish the new Secret, verify
SANs and validity, then roll every consumer. For a CA change, first publish a
bundle containing old and new roots and roll all peers. Next issue and roll new
leaf certificates. Remove the old root only after old leaves are gone, then
roll again. Coordinate this sequence with external PostgreSQL, Redis, ingress,
and Collector operators.

PASETO rotation has a stricter limitation: the token manager accepts exactly
one symmetric key and there is no key ID or dual-verification window. A normal
two-replica rolling update would temporarily mix keys and produce intermittent
`401` responses. Use a maintenance cutover or traffic-isolated blue/green
deployment so all token minting and validation uses one key at a time. Existing
access tokens become invalid after cutover; database-backed refresh tokens can
obtain new access tokens. Record and test the expected authentication impact
before rotation.

## Troubleshooting

Start with cluster state and structured application logs:

```bash
kubectl -n commerce get pods,deployments,jobs,services,endpoints
kubectl -n commerce get events --sort-by=.metadata.creationTimestamp
kubectl -n commerce describe pod <pod>
kubectl -n commerce logs <pod> --previous
kubectl -n commerce logs job/commerce-migrate
```

| Symptom | Checks and actions |
| --- | --- |
| Image pull failure | Confirm the overlay no longer contains the invalid registry or zero digest, the digest exists for the node architecture, and registry credentials/admission policy permit it. |
| Migrator fails | Confirm the schema-owner URL uses `verify-full`, the CA path exists, DNS matches the certificate, `pgcrypto` can be created, and the database is not dirty. Inspect the external PostgreSQL logs; do not start application Deployments. |
| API startup probe fails | Inspect startup logs for configuration validation, unreadable certificate files, invalid PASETO key, database TLS, port bind, or metrics bind failure. The scratch image has no shell. |
| API liveness succeeds but readiness fails | Read the `/readyz` check map. `postgres` points to URL/TLS/schema/connection capacity. `identity_grpc` points to Service endpoints, DNS SAN, client CA, exactly-one SPIFFE URI SAN, allowlist, or user-service health. |
| External clients cannot connect | Verify the ingress namespace label, NetworkPolicy-capable CNI, Service endpoints, native HTTPS mode, HTTP certificate SAN, and passthrough or verified re-encryption configuration. No Ingress object is supplied. |
| Protected order/payment returns `503` | Check user-service endpoints and gRPC health, then mTLS CA/server name/client URI SANs and the one-second call budget. Invalid tokens are `401`; dependency failures are `503`. |
| Intermittent `401` after a Secret rollout | Check whether user-service replicas loaded different PASETO keys. End the mixed-key rollout using the coordinated rotation procedure; do not keep retrying with newly minted tokens across mixed replicas. |
| Worker restarts | Initial PostgreSQL or queue Redis ping failed, configuration/TLS is invalid, or the loopback metrics listener could not bind. After startup, inspect Asynq health warnings and archived-task metrics because probes do not test dependencies. |
| Queue backlog grows | Check worker logs, Redis persistence/memory/`noeviction`, queue name, task duration, database latency, and archived tasks. Increase concurrency only after recalculating DB/Redis capacity and validating lock behavior. |
| Product reads are slow | The production cache is disabled by default, or cache Redis is bypassing on timeout. Check PostgreSQL first. Do not make Redis authoritative to hide database saturation. |
| Metrics target is down | Confirm a same-Pod scraper exists and the application logged its loopback metrics listener. A cross-Pod ServiceMonitor cannot reach it directly. |
| Traces are absent | Confirm Pods were rolled with `otlp`, endpoint is HTTPS with no path, CA/mTLS/header Secrets are mounted, egress permits the Collector, and sampling is nonzero. Read exporter logs; readiness remains green during Collector failure. |
| Rollout is unschedulable | Compare requests and surge headroom, topology spread, PDBs, quotas, and sidecar resources. Avoid solving it by removing limits without load and failure testing. |

For an API diagnostic, port-forward its Service and query the real certificate
DNS name using `curl --resolve`, the issuing CA, and the actual HTTP SAN. This
tests hostname verification, unlike the Kubernetes HTTPS probe. Preserve the
returned `X-Request-ID` and correlate it with JSON logs and sampled traces.
