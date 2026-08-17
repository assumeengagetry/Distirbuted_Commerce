# Architecture

This document describes the Phase 8 runtime architecture and its current
boundaries. It is intentionally narrower than the project [README](../README.md):
the HTTP surface is defined by the [OpenAPI contract](../api/openapi/commerce.yaml),
the identity RPC by the [protobuf contract](../api/proto/identity/v1/identity.proto),
and deployment procedures by [operations.md](operations.md). Security controls
and residual risks are covered in [security.md](security.md).

## System context

The platform serves three kinds of actors:

- Customers register, authenticate, browse products, create orders, and pay
  from an internal account balance.
- Administrators manage products and inventory and can inspect all orders and
  payments. The repository does not provide an administrator bootstrap flow.
- Operators build artifacts, migrate the database, deploy workloads, inspect
  telemetry, and recover individual archived maintenance tasks.

The application depends on infrastructure outside its process boundary:

```text
Customer/Admin
      |
      | HTTPS
      v
Ingress or Gateway
      |
      +----------------+----------------+
      |                |                |
      v                v                v
 user-service     order-service    payment-service

 order-service -------- TLS 1.3 mTLS --------> user-service
 payment-service ------ TLS 1.3 mTLS --------> user-service

 user-service ---- Asynq produce ----> queue Redis
 job-worker <---- consume/schedule --- queue Redis

All four processes --------------------> PostgreSQL
order-service -- optional cache -------> cache Redis
all four -- optional OTLP/gRPC --------> OTel Collector
same-Pod scraper <---------------------- loopback /metrics
```

PostgreSQL, queue Redis, optional cache Redis, the OpenTelemetry Collector,
Prometheus-compatible storage, ingress/Gateway infrastructure, and certificate
issuers are external services. The Kubernetes manifests under
[`deploy/kubernetes`](../deploy/kubernetes/) do not deploy any of them.

## Runtime components

Four processes are resident workloads. The other two operational binaries are
invoked on demand.

| Component | Lifecycle | Interfaces | Main responsibilities |
| --- | --- | --- | --- |
| `user-service` | Resident; two replicas in the base Kubernetes deployment | HTTPS `:8081`, identity gRPC `:9091`, loopback metrics `:9101` | Registration, login, refresh rotation, logout, profiles, PASETO minting/validation, and best-effort cleanup enqueue |
| `order-service` | Resident; two replicas | HTTPS `:8082`, identity gRPC client, loopback metrics `:9102` | Product catalog, administration, inventory, order creation/history, and optional product-detail cache |
| `payment-service` | Resident; two replicas | HTTPS `:8083`, identity gRPC client, loopback metrics `:9103` | Internal account debit, payment creation/read, and pending-to-paid order transition |
| `job-worker` | Resident; one replica with `Recreate` strategy | Asynq consumer/scheduler and loopback metrics `:9104`; no business HTTP API | Periodic, bounded deletion of expired authentication sessions |
| `job-admin` | On demand; no Kubernetes workload is supplied | Queue Redis only; CLI stdout/stderr | List metadata for archived tasks and retry or delete exactly one task |
| `migrator` | One-shot Kubernetes Job | PostgreSQL only | Apply embedded migrations upward and reject a dirty schema version |

The `loopback-probe` binary is a container helper used by worker probes; it is
not another service. Composition roots live under [`cmd`](../cmd/). Each
resident process owns its logger, database or Redis clients, telemetry
providers, listener lifecycle, and shutdown budgets. A failure of any listener
owned by one process cancels and shuts down that whole process.

## Request and telemetry chains

### User HTTP

For a user-service request, native HTTP TLS terminates in the Go process. Gin
creates a local server span without accepting an external trace parent, assigns
or validates a request ID, applies security headers and recovery, and then
dispatches the route. Auth endpoints use a source-IP rate limiter. A protected
profile request validates the bearer token locally because only user-service
holds the symmetric PASETO key. Domain work uses a bounded pgx pool and reviewed
sqlc queries.

Registration commits the user, zero-balance account, authentication session,
and initial refresh-token digest in one PostgreSQL transaction. Login and
refresh similarly commit their session state before maintenance scheduling.
After a successful authentication mutation, user-service attempts a detached,
bounded Asynq enqueue. Queue failure does not reverse the committed auth result;
one bounded inline PostgreSQL cleanup batch is attempted instead.

### Order and payment HTTP

A protected order or payment request follows this chain:

```text
HTTPS request
  -> request ID, local trace root, security headers, panic recovery
  -> source-IP rate limit
  -> bearer extraction
  -> identity.v1 ValidateAccessToken over TLS 1.3 mTLS
  -> principal rate limit and route role check
  -> domain service
  -> current user role/status check in PostgreSQL
  -> bounded PostgreSQL transaction or read
  -> sanitized HTTP response
```

The identity call has a one-second default upper bound, preserves a shorter
parent deadline, disables configured gRPC retries, and fails closed. An invalid
token maps to `401`; an unavailable or malformed identity response maps to
`503`. The RPC only validates token cryptography and claims. Sensitive domain
operations therefore re-read current role and status from PostgreSQL rather
than treating the token role as current state.

The payment transaction crosses account, order, idempotency, balance-entry,
and payment tables. No RPC, Redis operation, task enqueue, password hash, or
telemetry export occurs inside a business database transaction.

### Redis cache

Only public product-detail reads can use cache Redis. Order lists, admin reads,
pricing, inventory decisions, order creation, and payment never use cached data
as authority. A cache miss or invalid value reads PostgreSQL, then performs a
generation-fenced fill. Product and inventory changes invalidate after the
database operation. Redis timeout, malformed data, or outage causes a bypass to
PostgreSQL; it does not make order-service unready.

The production base leaves the product cache disabled. If enabled, cache Redis
must be a separately operated failure domain from queue Redis. Logical Redis
database numbers are a local-development convenience, not production
isolation.

### Asynq maintenance

User-service and the worker produce the versioned
`auth:prune-expired-sessions:v1` task. The worker schedules it every minute by
default and processes it with at-least-once delivery, a bounded execution time,
and up to five retries for ordinary failures. The payload is fixed and strict;
unknown or malformed tasks skip retry and are archived. Deleting expired
sessions is idempotent, so duplicate execution is acceptable.

The queue is not a transaction log for business state. Request-triggered
enqueue is deliberately best effort, and there is no transactional outbox.
Future authoritative external side effects must not reuse this guarantee.

### Telemetry

Every resident process has an isolated Prometheus registry and a plaintext
HTTP `/metrics` listener bound to Pod or host loopback. Metrics do not share the
business listener. In Kubernetes, only a process in the same Pod network
namespace, such as a sidecar, can scrape that loopback address directly.

Trace export is optional. When enabled, the request context propagates by W3C
Trace Context from an application-created HTTP root through identity gRPC, pgx,
cache Redis, and request-triggered Asynq tasks. Baggage is not propagated.
Periodic jobs start new roots. Queue Redis command tracing is disabled to avoid
polling noise; cache Redis tracing omits command text, keys, and caller data.
Structured logs created with a span context include `trace_id` and `span_id`.

OTLP export is asynchronous and bounded. The Collector is not a readiness
dependency and is not deployed by this repository, so traces may be delayed or
dropped during exporter or Collector failure while business traffic continues.

## Data ownership today

Ownership is logical at the Go package and process boundary, but physical data
ownership is not isolated by database or schema.

| Logical owner | Primary records | Cross-boundary access |
| --- | --- | --- |
| User/identity | `users`, `auth_sessions`, `refresh_tokens`; creates `accounts` during registration | Worker deletes expired sessions. Order and payment read user role/status. Payment mutates accounts. |
| Order | `products`, `inventories`, `orders`, `order_items`; order-operation rows in `idempotency_keys` | Payment locks and changes an order from `pending` to `paid`. |
| Payment | `payments`, `account_balance_entries`; payment-operation rows in `idempotency_keys` | Payment debits user-created accounts and relies on order-owned totals/status. |
| Migrator | The complete schema, constraints, triggers, and extensions | A single ordered migration stream serves every process. |

The production Secret contract supplies distinct runtime database URLs, so an
operator can grant different roles. The repository does not define those SQL
roles or grants, and the schema contains cross-domain foreign keys, triggers,
and transactions. A shared PostgreSQL instance remains the authoritative
consistency boundary.

## Consistency model

- PostgreSQL is authoritative. Transactions use `READ COMMITTED` plus explicit
  row locks, deterministic lock ordering, conditional updates, statement and
  lock timeouts, foreign keys, checks, uniqueness, and deferred settlement
  constraints.
- Product and inventory writes use optimistic versions. Order creation locks
  product rows in sorted UUID order and conditionally deducts inventory, so a
  failed later step rolls back all deductions.
- Order and payment creation reserve and complete a request hash and resource ID
  in the same transaction as the mutation. The client must retry an ambiguous
  result with the same `Idempotency-Key` and identical request.
- Payment locks the order and account, records the exact balance transition,
  inserts an immutable succeeded payment, and marks the order paid in one
  transaction. A lost commit acknowledgement is resolved by reading the
  completed idempotency record under an independent short deadline; the debit
  is never automatically repeated.
- Refresh rotation serializes on database rows. PostgreSQL stores only refresh
  token SHA-256 digests. Confirmed replay revokes the session family with a
  short context detached from client cancellation.
- Access tokens are stateless snapshots until expiry. Repository authorization
  checks limit stale role/status use for implemented domain operations, but an
  individual access token cannot be revoked immediately.
- Cache state is eventually consistent and bounded by its TTL when
  invalidation fails. Queue state is at least once. Neither is authoritative.

## Failure model

| Failure | Behavior and operator implication |
| --- | --- |
| PostgreSQL unavailable | API startup fails or readiness becomes false; business reads/writes fail. Worker startup fails, and active tasks retry/archive according to Asynq policy. There is no application-side database failover controller. |
| Identity gRPC unavailable | Protected order/payment requests fail closed with `503`, and those services become unready. Public product reads and liveness remain available. |
| Cache Redis unavailable | Product-detail requests bypass to PostgreSQL. Failed post-write invalidation can leave a stale value until TTL; authoritative mutations are unaffected. |
| Queue Redis unavailable | User auth commits remain successful and use bounded inline cleanup after enqueue failure. Worker cannot start, or logs queue health failures after startup. Cleanup can lag, but expired sessions are still rejected by database time checks. |
| Process termination | Kubernetes removes unready/terminating API Pods from traffic. HTTP, gRPC, worker, metrics, and telemetry shutdown are bounded; work exceeding the grace period can be interrupted and retried where the protocol permits. |
| Client timeout around a mutation | Ordinary transaction failure rolls back. If commit outcome is unknown, the service resolves the idempotency record or returns `OPERATION_OUTCOME_UNKNOWN`; the client retries the same key and body. |
| Collector or metrics scraper unavailable | Business readiness is unchanged. Metrics remain available on loopback; the bounded trace queue may drop data. |
| Zone, node, or dependency partition | Kubernetes can replace application Pods, but availability and split-brain controls for external PostgreSQL and Redis belong to their operators. PDBs cover only voluntary API Pod disruption. |

Pending orders consume inventory and have no expiry, cancellation, or automatic
restock workflow. This is a business-level failure mode, not an eventual
consistency mechanism.

## Why this is not a set of fully independent microservices

The code has useful service boundaries: separate binaries, listeners,
Deployments, configuration, telemetry registries, domain packages, and a narrow
identity RPC. Those boundaries permit independent API scaling and isolate the
PASETO key to user-service.

They do not provide independent data ownership or release autonomy. All
services share one schema and migration stream. Payment correctness relies on
one ACID transaction spanning user-owned accounts and order-owned records, and
database constraints directly connect those tables. Order and payment also
depend synchronously on user-service for protected traffic. Redis and Asynq are
supporting mechanisms, not a domain-event backbone.

Splitting the database now would weaken the implemented debit/order invariant
unless the design also introduced durable domain events, a transactional
outbox, explicit reservations, idempotent consumers, compensating actions, and
an observable saga. Until that work exists, the accurate description is a
modular distributed application with service-shaped processes and a shared
transactional core, not fully autonomous microservices.
