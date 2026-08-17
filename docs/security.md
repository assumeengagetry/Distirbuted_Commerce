# Security

This document is a threat-oriented description of the Phase 9 security
posture. It records implemented controls and known gaps; it does not assert that
the software is vulnerability-free. Operational details are in
[operations.md](operations.md), system boundaries in
[architecture.md](architecture.md), and the externally visible behavior in the
[OpenAPI contract](../api/openapi/commerce.yaml). Trusted publication and image
admission are in [release.md](release.md); external recovery and credential
rotation are in [backup-restore.md](backup-restore.md).

## Assets

The primary assets are:

- Passwords in transit, Argon2id password hashes, PASETO encryption and
  authentication key material, bearer access tokens, opaque refresh tokens, and
  refresh-token digests.
- Customer identity and profile data, account balances, balance history,
  orders, immutable order-item snapshots, payments, inventory, and
  idempotency records.
- PostgreSQL and Redis credentials, TLS private keys, CA trust bundles, OTLP
  authorization headers, and operator credentials.
- Queue contents and archive metadata. Current payloads are maintenance-only,
  but queue control can delay or delete cleanup work.
- Logs, metrics, and traces. They intentionally omit several sensitive fields
  but still contain operational metadata and must be access controlled.
- Source, migration history, protobuf/OpenAPI contracts, OCI images, image
  digests, and CI artifacts used to establish release provenance.

PostgreSQL is the authoritative data asset. Redis cache entries and queue state
are not substitutes for database recovery. The external database, Redis
services, Collector, and telemetry stores need independent backup, access,
patching, audit, and incident-response controls. The repository does not replace
provider backup; see [backup-restore.md](backup-restore.md).

## Trust boundaries

| Boundary | Expected control | Residual concern |
| --- | --- | --- |
| Internet client to ingress/Gateway | Public HTTPS, edge abuse controls, explicit routing | No ingress/Gateway or WAF is supplied by the repository. Application rate limits are per process. |
| Ingress/Gateway to API Pod | Native TLS 1.3; passthrough or verified HTTPS upstream; NetworkPolicy allows only a labeled trusted namespace | Kubernetes HTTPS probes do not verify certificates. A mislabeled namespace or unverified re-encryption weakens this boundary. |
| Order/payment to user identity RPC | TLS 1.3 mTLS, server DNS verification, client CA verification, exact SPIFFE URI allowlist, and ingress NetworkPolicy | The URI is SPIFFE-formatted but there is no SPIRE agent, workload API, or automated workload attestation. CA issuance remains external. |
| Application Pod to PostgreSQL | `sslmode=verify-full`, explicit CA, separate runtime/migrator URLs | All domains share one schema. Effective least privilege depends on external SQL grants not defined here. |
| User/worker to queue Redis | ACL password, TLS 1.3 server verification, optional client certificate support | Queue Redis is external. Persistence, ACL scope, failover, and restore quality are operator responsibilities. |
| Order to optional cache Redis | TLS 1.3 and strict cache validation; PostgreSQL fallback | Cache invalidation can fail and leave bounded stale data. The production base keeps this dependency disabled. |
| Pod to OTel Collector | HTTPS OTLP/gRPC, optional mTLS and Secret-backed headers | Exported spans leave the application trust domain. The Collector and its filtering/storage are not supplied. |
| Same-Pod scraper to metrics | Loopback-only listener, no Kubernetes Service | A compromised sidecar shares the Pod network namespace and can read metrics. |
| Operator/CI/secret controller to cluster | External IAM, review, audit, immutable digests, scoped secret delivery | The repository cannot enforce the surrounding organization, registry, cluster, or cloud controls. |
| Application processes to shared database | Process-specific URLs can map to separate roles | Payment intentionally crosses user and order records, so a compromised database role may have wider blast radius than a fully isolated service. |

## Authentication

### Passwords

Registration accepts bounded passwords and stores only PHC-formatted Argon2id
hashes with unique salts. Verification rejects hostile or excessive PHC
parameters before allocation. A process-wide semaphore limits concurrent
Argon2id work and returns `503` on saturation rather than allocating without
bound. Unknown-email login performs a dummy hash comparison, and malformed
email, unknown user, disabled user, and wrong password converge on a generic
credential failure where appropriate.

These controls reduce offline cracking and user enumeration; they do not
replace password screening, credential-breach detection, MFA, bot management,
or an account recovery process. Those features are not implemented.

### Access tokens

Access tokens are PASETO v4.local tokens. Validation requires issuer, audience,
subject, token ID, issue/not-before/expiry times, token type, and a known role.
The default lifetime is 15 minutes with bounded clock skew. Only user-service
loads the 32-byte symmetric key. Its HTTP API validates locally; order and
payment pass the raw bearer value to user-service over the internal mTLS RPC
and receive only typed principal fields.

The token is encrypted and authenticated, not individually revocable. Logout
revokes the refresh session but an already issued access token remains usable
until expiry. A user-service key compromise permits token decryption and
minting. There is no multi-key verification window, so rotation invalidates
existing access tokens and must avoid replicas running different keys. See the
[rotation procedure](operations.md#certificate-and-key-rotation).

### Refresh tokens

Refresh tokens are opaque random values. PostgreSQL stores only
`SHA-256(raw_token)`. Rotation locks the token/session row, uses database time,
consumes the old token, and inserts one replacement in one transaction.
Concurrent use is serialized. Reuse outside the short grace period revokes the
entire session family with a context detached from client cancellation.
Sessions have an absolute expiry that rotation cannot extend.

Hashing a high-entropy token protects against direct reuse of a database dump,
but it does not protect a raw token stolen from a client, request path, process
memory, or compromised TLS endpoint. Client storage and browser-specific CSRF
controls are outside this API repository.

## Authorization

Self-registration always creates a `customer`; request data cannot select a
role. Gin middleware centralizes route role checks. Order and payment first
authenticate through identity gRPC, then repositories re-read current
role/status from PostgreSQL and apply ownership predicates or admin access.
Product and inventory mutation requires a current admin. Order/payment creation
requires a current active customer. Customer reads are constrained by user ID.

The database also enforces foreign keys, uniqueness, nonnegative balances and
inventory, payment-to-debit linkage, immutable payments, and terminal paid
orders. These are defense-in-depth consistency controls, not a replacement for
application authorization.

The Kubernetes service accounts have token automount disabled and no repository
RBAC bindings. They do not establish end-user identity. Database least privilege
must be created externally for the five URLs in the
[Secret contract](../deploy/kubernetes/overlays/production/secrets-contract.md).
Because the current payment transaction updates account and order state, its
role necessarily crosses logical domain ownership.

## Transport security

- Production HTTP configuration requires a certificate/key pair and uses TLS
  1.3 or newer in the Go server. Plaintext HTTP is accepted only on loopback in
  non-production environments.
- Production identity gRPC requires TLS 1.3 mutual authentication. Clients
  verify the user-service DNS SAN. The server verifies the client chain and
  requires exactly one URI SAN present in its explicit allowlist.
- Production PostgreSQL URLs must use `sslmode=verify-full`; Unix sockets,
  unverified TLS targets, and insecure fallback targets are rejected by API and
  worker configuration. The production migrator independently requires
  `verify-full`.
- Production Redis configuration requires a CA and server name and uses TLS
  1.3. A client certificate/key pair is supported but optional.
- Production OTLP export requires HTTPS and uses TLS 1.3, with system or
  explicitly configured roots and an optional client certificate pair.
  Plaintext OTLP is limited to loopback outside production.
- Certificate and CA files are read at startup. Secret projection alone does
  not rotate live listeners or clients.

HTTP client certificate authentication is not implemented. Public client
authentication is bearer-token based. The edge must not terminate HTTPS and
then forward unverified plaintext to a Pod.

## Secret handling

Local `make init` creates `.env` and `.secrets/` with owner-only permissions,
does not print generated values, and Git ignores those paths. Container build
ignore files exclude local configuration, secrets, deployment material, docs,
and repository metadata from the build context copied into an image.

Kubernetes manifests reference externally managed Secrets but deliberately do
not create them. TLS and CA Secrets are mounted read-only with mode `0440` and
Pod GID 65532. Database URLs, the PASETO key, and the queue Redis password enter
selected containers as individual environment variables. No Pod uses
`envFrom` on the runtime Secret, and order/payment/worker reject a configured
PASETO key.

Kubernetes Secrets are not encrypted merely because they are base64 encoded.
Production must use encrypted etcd or a KMS-backed secret controller, tightly
scoped human and controller RBAC, audit logs, backup protection, and namespace
access review. Avoid exposing environment blocks through debug tooling, crash
dumps, support bundles, or broad Pod-exec permissions. A privileged node or
cluster administrator remains able to obtain Pod secrets.

`commerce-runtime` groups several high-value keys in one Secret object even
though Pods select individual keys. Access to that whole Secret has broad blast
radius. Separating Secret objects by workload is a reasonable environment
hardening step, but it requires a reviewed manifest change rather than an
assumption that the current contract already provides object-level isolation.

## Network, container, and Kubernetes controls

The [Containerfile](../Containerfile) pins its builder base by digest and emits
a minimal scratch runtime. It removes compiler and shell tooling, runs as
`65532:65532`, and includes only the service, loopback probe, and public CA
bundle. Kubernetes adds `runAsNonRoot`, RuntimeDefault seccomp, a read-only root
filesystem, all-capability drop, no privilege escalation, resource limits, and
disabled service-account token automount.

ClusterIP Services expose only native HTTPS and identity gRPC. Metrics stay on
loopback. A default-deny ingress policy applies in namespace `commerce`; API
ingress is limited to a namespace with an explicit label, and identity gRPC is
limited to order/payment Pods. Worker and migrator have no allowed inbound
application traffic.

The base does not define default-deny egress, destination-specific dependency
egress, namespace Pod Security Admission labels, an ingress/Gateway, runtime
sandboxing beyond RuntimeDefault seccomp, or a NetworkPolicy for external
provider addresses. A fail-closed
[Kyverno image policy](../security/policies/kyverno/verify-images.yaml) is
supplied separately but is cluster-scoped, optional, and not installed by the
application overlay. NetworkPolicy and admission effectiveness depend on the
external cluster. Add, operate, and test those controls in the target
environment; do not infer them from the application manifests.

Production image names and digests in the overlay are placeholders. Applying
them unchanged is a release failure. Normal CI builds images without publishing
them. The separate trusted `v*` tag workflow publishes to GHCR, scans final
images with Trivy, generates Syft SPDX SBOMs, signs digests with keyless Cosign,
and publishes GitHub provenance and SBOM attestations. GitHub rulesets,
environment approval, registry retention, long-term evidence custody, Kyverno
installation, and overlay promotion remain external controls; see
[release.md](release.md).

## Application input and abuse controls

- JSON bodies are limited to 16 KiB and must be one strict object with the
  expected media type, valid UTF-8, lowercase keys, no unknown or duplicate
  fields, and no trailing value.
- HTTP and gRPC header, message, stream, read, write, operation, lock, and
  shutdown bounds constrain resource retention.
- Gin trusts no forwarding proxy. Source IP is the direct peer unless an
  explicitly designed edge integration changes this behavior.
- Auth, commerce, and payment token buckets are bounded in entry count and TTL.
  Commerce/payment protected routes rate-limit both direct IP and principal.
- Idempotency keys are bounded, validated, hashed before storage, and excluded
  from normal logs. Request hashes bind a key to canonical mutation content.
- SQL is reviewed and generated through sqlc; error envelopes do not expose
  pgx, SQL, Argon2id, token parser, TLS, or dependency internals.
- Pagination cursors, UUIDs, cache entries, task payloads, roles, statuses, and
  monetary ranges are strictly validated.

Rate limiting is process-local. Adding replicas increases aggregate allowance,
and source-IP behavior behind a proxy must be designed at the edge. There is no
distributed limiter, CAPTCHA, WAF, or automatic autoscaler in this repository.

## Telemetry data constraints

Public HTTP starts a local trace root and ignores inbound trace IDs and sampling
flags, preventing an external caller from choosing internal trace identity.
Only W3C Trace Context is propagated internally; baggage is not accepted or
forwarded. Request-triggered Asynq tasks carry trace headers separately from
their deterministic payload.

Custom metric dimensions are restricted to route templates, methods, statuses,
fixed outcomes, pool state, and static resource attributes. The metric SDK has
a cardinality limit and disables exemplars. PostgreSQL instrumentation omits
SQL statements and connection details. Redis cache tracing omits command text,
keys, and caller details, and queue Redis command tracing is disabled.

These controls reduce exposure but do not make telemetry public data. Logs can
contain request IDs, product IDs, dependency targets, archived-task errors, and
stack traces. Library-generated spans can contain protocol and URL metadata.
Apply Collector-side allowlists/redaction, authenticated export, tenant
isolation, least-privilege query access, bounded retention, and incident review.
Never add passwords, bearer/refresh tokens, PASETO keys, idempotency keys, raw
payloads, SQL parameters, Redis keys, DSNs, emails, IPs, or unbounded error text
as metric labels or custom span attributes.

OTLP authorization headers are Secrets. The external Collector and telemetry
storage are outside this repository and must be threat-modeled as data
processors. Trace sampling is a cost/privacy control, not an access control.

## Threats and residual risks

| Threat | Existing controls | Residual risk |
| --- | --- | --- |
| Credential stuffing and password guessing | Argon2id, generic failures, dummy verification, bounded concurrency, per-IP limits | Distributed attackers and replica multiplication can bypass local limits; no MFA, breached-password check, or edge bot control |
| Access-token theft or forgery | Authenticated encryption, strict claims, short TTL, TLS, key isolation to user-service | No individual access-token revocation; symmetric key compromise grants minting authority; client storage is out of scope |
| Refresh-token replay | One-time digests, row locking, constant-time comparison, family revocation, absolute expiry | Raw client-side token theft remains usable until detected or expired; no user-facing session inventory |
| Broken object or function authorization | Central middleware, current database role/status checks, owner-filtered queries, schema constraints | Authorization defects are still possible; no formal policy engine or complete audit history |
| Injection or malformed input | Strict JSON/query parsing, sqlc queries, bounded bodies and headers, sanitized errors | Parser/library defects and future ad hoc queries remain possible; contract validation is not a proof of runtime conformance |
| Oversell, duplicate order, or double debit | Deterministic locks, conditional updates, idempotency records, balance ledger, deferred constraints | Pending orders have no expiry/cancel/restock; no external payment provider, refund, or reconciliation workflow |
| Service impersonation or lateral movement | TLS 1.3, identity mTLS, DNS and URI SAN checks, ingress NetworkPolicy, no service-account tokens | CA or cluster compromise defeats the boundary; no SPIRE attestation; no default-deny egress |
| Redis compromise or loss | TLS, ACL password, strict cache/task schemas, DB authority, idempotent cleanup | Queue delay/loss affects cleanup; production persistence/backup and ACL least privilege are external; cache can be stale |
| PostgreSQL compromise | Verified TLS, process-specific URL contract, hashes instead of raw refresh/idempotency values, constraints | Shared schema and cross-domain grants increase blast radius; database encryption, HA, backup, and audit are external |
| Denial of service | Timeouts, body/message caps, rate limits, Argon semaphore, pool bounds, container limits | Database locks, connection exhaustion, expensive auth, queue backlog, and edge floods can still exhaust capacity; no HPA/WAF |
| Telemetry exfiltration | Loopback metrics, bounded labels, omitted SQL/Redis details, optional authenticated OTLP | Logs and spans remain sensitive; Collector/storage policy is external; operator-added attributes can regress privacy |
| Supply-chain compromise | Pinned Go modules and actions, digest-pinned builder, tests, `govulncheck`, protected-tag release contract, Trivy image gate, Syft SPDX SBOM, keyless Cosign signature, GitHub attestations, immutable deployment digests, and optional Kyverno policy | No claim of complete vulnerability detection; tag rules, environment approval, GHCR retention, Sigstore availability, evidence custody, and Kyverno operation are external |
| Operator misuse | No bulk task mutation, single-task lock, separate migrator/admin binaries, immutable payment records | Cluster/database operators remain highly privileged; job deletion and schema changes need external approval and audit |
| Best-effort async delivery | Current task is idempotent, periodic scheduling, retries/archive, inline fallback | There is no transactional outbox; this mechanism is unsuitable for authoritative external side effects |

Additional product gaps include email verification, password reset, MFA,
account deletion, administrator bootstrap, customer funding, refunds,
cancellation, automatic restock, security audit history, dashboards, alerting,
and repository-provided backup or disaster-recovery automation. Their absence
must be included in deployment risk acceptance; the external recovery contract
is documented in [backup-restore.md](backup-restore.md).

## Release security checklist

1. Run `make release-check` against isolated disposable PostgreSQL and Redis,
   and require the current [CI workflow](../.github/workflows/ci.yml) to pass.
   Review `govulncheck` results as one signal, not proof that no vulnerability
   exists.
2. Review changes to the [OpenAPI contract](../api/openapi/commerce.yaml),
   [identity protobuf](../api/proto/identity/v1/identity.proto), migrations, auth
   logic, authorization queries, secret references, and NetworkPolicies.
3. Complete the [trusted release verification](release.md#verification-and-evidence),
   including the Trivy result, Syft SPDX SBOM, Cosign signature, both GitHub
   attestations, and externally retained evidence. Independently scan or review
   risks not covered by the enforced gate.
4. Replace every production image and dependency placeholder. Run
   `make kube-check` and verify rendered output contains no Secret resources or
   secret values.
5. Verify the external
   [backup and restore contract](backup-restore.md#restore-drills), migration-role
   separation, runtime grants, `verify-full` URLs, Redis
   persistence/`noeviction`, ACLs, TLS names, and capacity headroom.
6. Verify HTTP SANs, identity server DNS SAN, exactly-one client SPIFFE URI SAN,
   CA bundles, expiry windows, Secret ownership, encrypted secret storage, and
   audited secret-controller access.
7. Verify trusted ingress namespace labels, TLS passthrough or verified
   re-encryption, CNI NetworkPolicy enforcement, destination-specific egress,
   and absence of unintended public Services.
8. Complete [overlay promotion](release.md#overlay-promotion), apply the
   [migration-first rollout](operations.md#migration-first-rollout), then test
   readiness, authentication failure modes, customer ownership, admin
   denial/allow paths, and same-key idempotent replay before full traffic.
9. Verify loopback metrics collection, alert routing, OTLP authentication,
   sampling, Collector redaction, retention, and that no secret or high-cardinality
   attribute was introduced.
10. Record previous image digests, schema compatibility, rollback criteria,
    key/certificate rotation impact, accepted residual risks, release approvers,
    and incident contacts.

Passing this checklist reduces known deployment risk. It cannot establish the
absence of implementation, dependency, configuration, infrastructure, or
operational vulnerabilities.
