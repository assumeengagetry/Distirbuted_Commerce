# Backup, restore, and rotation

This Phase 9 runbook defines recovery and credential-rotation requirements for
the production deployment contract. PostgreSQL and Redis are external services;
the repository does not deploy a backup controller, archive WAL, take provider
snapshots, copy Redis persistence files, store backup encryption keys, or invoke
a provider restore API. These procedures supplement, and cannot replace, the
provider's supported backup and disaster-recovery service.

PostgreSQL is authoritative for identity, balances, products, inventory,
orders, payments, and idempotency. Queue Redis contains Asynq maintenance work.
Cache Redis is disposable and must not become a recovery source.

Read [operations.md](operations.md) for deployment ordering and
[release.md](release.md) for immutable image and release-evidence requirements.

## Recovery objectives and ownership

Before production use, the service owner, database owner, Redis owner, security
owner, and incident commander must agree and record:

- PostgreSQL, queue Redis, and secret-system RPO and RTO values.
- Full-backup frequency, PITR window, WAL retention, Redis persistence mode,
  backup retention, geographic and account separation, and legal holds.
- Backup encryption, KMS ownership, key recovery, access review, audit logging,
  and deletion protection.
- Named restore operators and approvers, provider escalation paths, and
  break-glass access.
- The order for stopping writes, restoring dependencies, rotating credentials,
  running migrations, starting workloads, and reopening traffic.
- Evidence retention for backup jobs, restore drills, production recoveries,
  credential rotations, and destructive approvals.

Monitor backup age, failed jobs, WAL archive delay, PITR coverage gaps, storage
capacity, replication health, Redis persistence errors, and restore-drill age.
A successful provider status flag is not enough; only a restore proves that the
backup can be consumed.

## PostgreSQL backup policy

Use the external provider's supported physical backup and PITR mechanism. The
minimum production contract is both:

1. Regular, transactionally consistent full base backups or provider full
   snapshots.
2. Continuous WAL capture sufficient to restore to a selected transaction time
   between retained full backups.

A logical `pg_dump` can be a useful independent export, but it is not a
substitute for full physical backup plus WAL-based PITR. Kubernetes volume
snapshots are not relevant unless the external PostgreSQL operator explicitly
coordinates crash-consistent database snapshots and restore. Copying a live
PostgreSQL data directory is not a backup procedure.

The external backup design must:

- Encrypt backups in transit and at rest with keys recoverable independently of
  the failed database account.
- Keep at least one protected copy in a separate account or failure domain and
  prevent an application, CI, or routine database role from deleting it.
- Retain complete full-backup and WAL chains for the declared PITR window.
- Include or separately preserve database roles, grants, ownership, required
  extensions such as `pgcrypto`, parameter settings, certificate dependencies,
  and provider configuration needed to reproduce the service.
- Preserve `schema_migrations`; it is part of the database recovery state.
- Record provider backup IDs, start and completion times, recoverable time
  bounds, encryption key identity, software version, and verification status
  without recording credentials.
- Use provider-supported consistency checks, backup validation, and corruption
  detection. Checksums of exported metadata do not prove database consistency.

Take or confirm a recoverable point immediately before an approved migration or
other high-risk data change. The release record should reference the external
backup identifier and latest recoverable time, not contain backup data or
decryption keys.

## PostgreSQL restore

Prefer restore to a new, isolated database instance. Do not overwrite the
suspected source or destroy its forensic state. A production restore is a data
loss and traffic-routing decision, not merely a database command.

1. Declare the incident, stop application writes and migration activity, and
   preserve database, deployment, and provider evidence. Pause API traffic,
   user-service queue production, and the worker when their activity could
   alter the recovery decision.
2. Obtain recorded approval for the selected full backup and PITR target. Pick a
   time before the unwanted transaction or corruption while accounting for
   clock source and observed commit time. State the expected data-loss window.
3. Ask the provider or authorized database operator to restore into an isolated
   endpoint using the supported full-backup and WAL chain. Keep the original
   instance read-only or isolated according to the incident plan.
4. Validate provider restore status, PostgreSQL version and extensions, TLS
   hostname and CA, roles and grants, database ownership, connection limits,
   and the clean migration version before any application connects.
5. Perform read-only consistency checks. Reconcile representative users,
   accounts, balance history, products and inventory, orders and order items,
   payments and debit entries, idempotency records, and foreign-key and deferred
   constraint behavior. Compare expected counts and business checkpoints with
   evidence from before the incident without exposing customer data.
6. Determine which immutable application and migrator digests are compatible
   with the restored schema. Do not automatically run the newest migrator just
   because the restore completed.
7. Recreate or rotate least-privilege runtime credentials, update externally
   managed Secrets with the new verified endpoint, and execute the
   [migration-first rollout](operations.md#migration-first-rollout) only if the
   compatibility decision requires it.
8. Resolve queue Redis using the queue recovery procedure below. PostgreSQL and
   Redis backups are not transactionally coordinated, so expect missing or
   repeated maintenance tasks.
9. Start user-service first, then order-service, payment-service, and the worker.
   Keep public traffic closed while testing readiness, authentication, current
   balances, controlled idempotent order/payment replay, and worker behavior.
10. Reopen traffic gradually, monitor database and queue error rates, and record
    the achieved recovery point, measured data loss, RTO, final schema version,
    credentials rotated, approvals, and unresolved reconciliation work.

Never validate a restore by pointing destructive integration tests at it.
`make test-integration` recreates its designated test database and flushes its
designated Redis database.

## Queue Redis persistence and recovery

Production queue Redis is external and must be dedicated to Asynq, configured
with bounded memory and `noeviction`, and operated with provider-supported
persistence and backups. The Kubernetes manifests only configure clients; they
do not make Redis durable.

Choose and document a persistence design that meets the queue RPO. Normally
this includes AOF with an explicitly accepted fsync policy, periodic RDB or
provider snapshots, durable storage, replication or failover as required, and
off-instance backup retention. Validate the exact Redis and Asynq versions,
database number, ACLs, TLS identity, memory policy, and persistence settings on
restore. An in-memory replica without durable and independently recoverable
state is not a backup.

Current queue payloads schedule idempotent expired-session cleanup, and
PostgreSQL remains authoritative. Queue loss can delay cleanup; restored queue
state can replay cleanup. This limited property must not be generalized to
future tasks that perform authoritative or external side effects.

For planned queue backup or migration, quiesce producers and workers when the
provider requires it and capture the acknowledged persistence position. For an
incident restore:

1. Stop workers and queue producers, preserve the failed service and persistence
   evidence, and select a provider-supported AOF, RDB, or managed backup under
   the declared RPO.
2. Restore to an isolated, dedicated Redis endpoint. Do not copy individual
   Asynq keys, edit an AOF manually, or use a cache snapshot as queue state.
3. Validate persistence loading, Redis version, `noeviction`, memory headroom,
   queue database and name, ACL least privilege, TLS CA and server name, and
   provider failover health.
4. Inspect pending, scheduled, retry, active, and archived task counts before
   starting a worker. Record unexpected task types or payload schema versions.
5. Compare the queue recovery time with the PostgreSQL recovery time. Classify
   tasks that may have completed in one system but not the other. Do not bulk
   retry or delete tasks to make counts look normal.
6. Start one worker against the recovered PostgreSQL and Redis endpoints.
   Confirm that current cleanup tasks remain idempotent, then restore normal
   worker operation and queue production.
7. Monitor duplicate execution, archive growth, Redis persistence errors, and
   PostgreSQL load. Record the recovered persistence object, data-loss window,
   task reconciliation, and approvals.

If recovery policy accepts rebuilding an empty queue for the current
maintenance-only workload, that is an explicit incident decision, not a Redis
restore. Preserve the failed queue first, document lost scheduled/retry/archive
state, start empty Redis with the correct durable settings, and let the worker's
periodic schedule and user-service fallback reestablish cleanup. Reassess this
decision whenever a new task type is introduced.

Never run `FLUSHDB`, `FLUSHALL`, broad key deletion, an unreviewed queue purge,
or bulk retry against production queue Redis. The provided `job-admin` supports
only inspected single-task actions for this reason.

## Cache Redis

Do not back up cache Redis. It is optional, disabled in the production base, and
derivable from PostgreSQL. Restore or provision it empty after PostgreSQL is
healthy, validate its eviction policy and TLS/ACL settings, and allow normal
reads to warm it.

Keep cache Redis separate from queue Redis. If a provider backup necessarily
restores both, isolate the queue evidence first and invalidate only the cache
keyspace under approval. Never flush a shared Redis service to clear cache data;
that can destroy Asynq state. Plan for the temporary PostgreSQL load increase
caused by an empty cache before reopening traffic.

## Restore drills

Run a full restore drill at least quarterly and after changing the database or
Redis provider, engine major version, topology, encryption keys, backup policy,
or restore automation. The organization may require a shorter interval based on
RPO, RTO, or regulation.

Each drill must use an isolated account, project, network, and DNS name with no
route from public production traffic. Use representative encrypted backups and
the same operator path used during an incident. Do not weaken access control or
copy production secrets into the drill environment without explicit approval.

Exercise and record all of the following:

- Restore the latest full PostgreSQL backup and separately restore a selected
  PITR target between full backups. Measure backup age, recovery-point error,
  and end-to-end RTO.
- Restore database roles, grants, extensions, TLS trust, and application
  connectivity. Confirm `schema_migrations` is present and clean before running
  any migrator.
- Run read-only consistency checks and a controlled application smoke test with
  the compatible immutable digests. Use synthetic drill data for mutations.
- Restore queue Redis persistence, inspect all Asynq states, and demonstrate
  controlled handling of a task that may be replayed. Also test the approved
  empty-queue path if it is part of the recovery policy.
- Start with an empty cache and measure database load during warm-up. No cache
  backup should be required.
- Exercise unavailable backup, broken WAL chain, missing KMS permission,
  invalid TLS identity, wrong Redis ACL, dirty migration, and exceeded-RTO
  escalation paths.
- Verify monitoring, incident communication, approvals, audit events, and
  evidence export. Record every manual deviation from the runbook.
- Destroy the drill environment and any temporary credentials under a reviewed
  cleanup ticket while retaining non-secret drill evidence.

A drill passes only when data checks, application behavior, queue handling,
security controls, and measured RPO/RTO meet the recorded objectives. Track
failed criteria to closure and repeat the affected scenario.

## Migration dirty state

The embedded migrator uses `golang-migrate`, applies only upward migrations, and
reports a dirty version when a migration started but did not complete cleanly.
It refuses to report success while dirty. The repository does not provide an
automatic production repair or force operation.

When a migration fails or reports dirty:

1. Stop the rollout and all application writes. Do not start new binaries and
   do not rerun the migration repeatedly.
2. Preserve migrator logs, PostgreSQL logs, the release and migration hashes,
   the rendered manifest, the dirty version, and a new external forensic backup
   or snapshot if the provider can take one without destroying prior recovery
   points.
3. Have the database owner compare the applied schema and data with the exact
   `up` migration at that version. Determine whether PostgreSQL rolled back the
   statement or transaction and whether any non-transactional effects remain.
4. Reproduce the state on a restored clone. Prefer a reviewed forward repair
   that makes the intended schema true and returns migration metadata to a clean
   state.
5. If forward repair is unsafe, choose a provider PITR point before the
   migration and explicitly approve the resulting data loss. Restore to a new
   instance and follow the full PostgreSQL recovery procedure.
6. Resume migration and rollout only after database and application owners
   approve the repaired schema, compatibility tests pass, and a rollback or
   recovery point remains available.

Changing the migration version with a force operation changes metadata; it does
not complete or reverse partially applied SQL. Direct edits to
`schema_migrations`, a force operation, a down migration, and
`make migrate-down` against production are dangerous operations requiring the
approval controls below. Down migrations can destroy data, and the Kubernetes
migrator supports only `up`.

## Credential rotation

All application database credentials, Redis credentials, PASETO keys,
certificates, private keys, and CA pools are loaded at process startup. Updating
a Secret does not update existing processes or established pools. Every rotation
requires a controlled rollout and proof that old material is no longer in use.

Do not place old or new credentials in release evidence, tickets, shell history,
rendered manifests, logs, or CI artifacts. Generate and store them in the
external secret or certificate system, record only secret versions or key IDs,
and verify workload-specific access.

### PASETO key

The token manager accepts exactly one 32-byte v4.local symmetric key, encoded as
64 hexadecimal characters. There is no key ID or dual-verification window.
Running user-service replicas with different keys causes intermittent `401`
responses.

Use an approved maintenance cutover or traffic-isolated blue/green deployment
that ensures all token minting and validation uses one key at a time. Existing
access tokens become invalid at cutover; valid database-backed refresh sessions
can mint access tokens under the new key. Verify that only user-service receives
the key, test refresh and login, and record customer impact. Rolling back the
old key will in turn invalidate access tokens minted with the new key, so make
the rollback decision explicit.

### TLS certificates and CAs

For a leaf certificate under an unchanged CA, issue the new certificate with
the exact required DNS or URI SANs, publish it through the external secret
system, verify chain and validity, and roll every listener or client that loads
it.

For a CA rotation, first publish a trust bundle containing old and new roots and
roll every verifier. Then issue and roll new server and client leaves. Confirm
that no old leaf remains before removing the old root and rolling verifiers
again. Coordinate PostgreSQL, Redis, ingress, identity gRPC, and optional OTLP
peers. An emergency compromise response may shorten overlap, but requires an
incident decision because it can cause an outage.

### PostgreSQL credentials

Rotate the five process-specific database URLs independently and retain
least-privilege separation. Prefer a provider-supported dual-credential method
or a replacement role with reviewed equivalent grants so the new credential can
be tested before the old role is revoked. PostgreSQL normally has one password
per role; an in-place password change therefore requires a coordinated Secret
update and rollout because existing connections can survive while later
reconnects with the old password fail.

For each workload, create and test the new credential with
`sslmode=verify-full`, the expected hostname, and the mounted CA; update only its
Secret key; roll the consumer; verify new database sessions and application
readiness; drain old sessions; then revoke the old password or role. Rotate the
migrator owner credential separately and verify ownership and DDL grants without
running a migration as a connectivity test. Never temporarily grant a runtime
role schema-owner privileges to simplify rotation.

### Redis credentials

Rotate queue and cache credentials independently. Redis ACL users can hold more
than one password, so add a new password first, update the external Secret, roll
all clients, verify new authenticated TLS connections, and remove the old
password only after old pools and Pods are gone. A second ACL user is an
alternative when provider policy requires distinct identities.

For queue Redis, coordinate user-service, job-worker, and any short-lived
`job-admin` workload. Password rotation must not flush data, reset unrelated ACL
rules, change the queue database, or disable persistence. If cache Redis is
enabled, use a separate ACL and Secret contract; do not reuse queue credentials.
Redis client certificates follow the TLS overlap procedure.

## Human approval for dangerous operations

Require a tracked change or incident record and explicit human approval before:

- selecting or changing a production PITR target, restoring over an existing
  database, failing over to a restored instance, or accepting data loss;
- deleting a database, table, backup, WAL chain, Redis persistence object,
  release evidence, image digest, signature, or attestation;
- editing `schema_migrations`, forcing a migration version, running a down
  migration, or applying manual production DDL or data repair;
- running Redis `FLUSHDB` or `FLUSHALL`, broad key deletion, queue purge, bulk
  retry, destructive ACL reset, or unreviewed persistence conversion;
- flushing a cache when the resulting database load can affect production;
- rotating or revoking the PASETO key, a trust root, database owner credential,
  backup KMS key, or break-glass credential;
- moving or deleting a protected release tag, deleting an approved GHCR digest,
  bypassing release evidence, or weakening fail-closed admission.

The record must identify the exact target and scope, reason, expected data or
availability impact, current backup and recovery point, tested procedure,
rollback or forward-recovery plan, observers, approvers, start and stop times,
commands or provider actions taken, and validation results. Use two-person
control for production data loss, migration metadata changes, trust-root or
PASETO rotation, backup deletion, and admission bypass. At minimum, include the
incident or change owner plus the responsible database, platform, or security
owner for the affected boundary.

Emergency approval can be expedited by the documented incident process, but it
must not be implicit. Preserve evidence and complete retrospective review after
service stabilization.

## Responsibility boundary

This repository documents expected behavior and supplies application-side
configuration checks. Provider selection, backup execution, WAL and Redis
persistence, replication, encryption, retention, restore infrastructure, secret
storage, certificate issuance, KMS recovery, approval enforcement, and disaster
recovery remain external operational responsibilities. A passing CI workflow,
release attestation, local volume, Redis AOF setting, or repository runbook does
not constitute a production backup or prove recoverability.
