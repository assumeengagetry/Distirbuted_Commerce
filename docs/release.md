# Release and promotion

This Phase 9 runbook covers trusted OCI publication, release evidence, image
admission, and promotion of immutable digests. The repository supplies the
[trusted release workflow](../.github/workflows/release.yml), a rendered
manifest verifier, and an optional
[Kyverno policy](../security/policies/kyverno/verify-images.yaml). It does not
configure GitHub rulesets, make GHCR undeletable, install Kyverno, update an
environment overlay, or deploy a release.

Read [operations.md](operations.md) for migration-first deployment and
[backup-restore.md](backup-restore.md) before a release that changes the
database. The recovery point must come from the external PostgreSQL provider or
backup platform; repository files and release artifacts are not database
backups.

## Release invariants

- A release starts from an annotated `v*` tag whose target commit is on
  `origin/main`.
- All six images are built from that one tag and source commit:
  `user-service`, `order-service`, `payment-service`, `job-worker`, `job-admin`,
  and `migrator`.
- A deployment refers to `ghcr.io/assumeengagetry/distributed-commerce/<service>@sha256:<digest>`.
  A tag is a discovery label, not a deployment identity. GHCR tags can be
  moved; an OCI digest identifies immutable content, although a registry
  administrator can still delete that content.
- Every matrix job must succeed before any digest from the run is approved.
  A partial matrix run is not a release.
- Promotion reuses the verified digest. It never rebuilds an image for a later
  environment.
- Schema compatibility, the previous release digests, and a tested external
  recovery point are known before production promotion.

The production overlay contains five resident or migration image references.
`job-admin` is published and verified with the release but is run only as a
short-lived administrative workload, so it is not present in the overlay.

## External prerequisites

Configure and test these controls outside the repository before creating a
release tag:

1. Protect `main` with review and required CI checks. The release workflow
   verifies that the tagged commit is an ancestor of `origin/main`; it does not
   query or enforce branch protection or CI conclusions.
2. Create a GitHub repository ruleset for `refs/tags/v*`. Restrict tag creation
   to release managers or an approved release automation identity, and block
   tag update and deletion. Require an annotated tag; require a signed tag if
   that is part of the organization's trust policy. The workflow checks that
   the ref resolves to a tag object but does not verify a Git tag signature.
3. Protect the GitHub `production` environment used by every publish job with
   required reviewers, deployment-branch or tag restrictions, and no
   self-approval where the plan supports it. These settings are not stored in
   this repository.
4. Permit GitHub Actions to mint OIDC tokens and write packages and
   attestations. Restrict workflow changes through `CODEOWNERS` or an equivalent
   review rule. The job intentionally receives `packages: write`,
   `id-token: write`, and `attestations: write`.
5. Provision the GHCR namespace, package visibility, pull credentials, and
   retention controls. Protect approved manifests and their signature,
   provenance, and SBOM referrers from deletion. Do not grant application
   deployers package-delete permission.
6. Allow the hosted runner to reach GHCR, GitHub attestation services, Fulcio,
   Rekor, Syft and Trivy data sources, and any GitHub endpoints required by the
   pinned actions. Define an outage policy that fails closed.
7. Provide an access-controlled evidence archive with retention that meets the
   audit and incident-response policy. GitHub workflow artifacts in this
   repository are retained for only 90 days.
8. Provision the target cluster, external PostgreSQL and Redis services,
   Secrets, certificates, ingress, network controls, and observability described
   in [operations.md](operations.md#production-prerequisites). Install and
   operate admission control separately if it is required.
9. Verify a recent full PostgreSQL backup, continuous PITR coverage, and the
   latest restore drill according to
   [backup-restore.md](backup-restore.md#postgresql-backup-policy). The
   repository cannot replace or validate provider-side backup storage by
   itself.

## Trusted tag workflow

After change review, required CI, migration review, and release approval, create
the protected tag at the exact reviewed commit. A signed annotated tag is the
preferred operator procedure:

```bash
TAG=v1.2.3
SHA=<reviewed-main-commit>
git fetch origin main --tags
git merge-base --is-ancestor "$SHA" origin/main
git tag -s "$TAG" "$SHA" -m "Release $TAG"
git push origin "refs/tags/$TAG"
```

Do not move or recreate a published tag. Correct a bad candidate with a new
version tag so that release identity and evidence remain unambiguous.

The tag push starts six independent `linux/amd64` matrix jobs. Each job:

1. Checks out full tagged history, requires the ref to be a tag object, confirms
   that its target is on `origin/main`, and confirms a clean checkout.
2. Builds the selected binary from the repository `Containerfile`, pushes the
   `v*` tag to GHCR, and captures the registry `sha256` digest from Buildx
   metadata.
3. Runs Syft 1.51.0 through the pinned Anchore SBOM action and writes an SPDX
   JSON document for the digest.
4. Runs Trivy 0.74.0 against that digest. Any `HIGH` or `CRITICAL` finding,
   including an unfixed finding, fails the job. The successful evidence bundle
   contains the SARIF output. This gate is not proof that lower-severity,
   application-level, or newly disclosed vulnerabilities are absent.
5. Uses Cosign 3.1.3 and GitHub OIDC to create a keyless signature for the image
   digest. No long-lived signing key is stored in the repository.
6. Uses `actions/attest` to publish GitHub SLSA v1 build provenance and a signed
   SPDX 2.3 SBOM attestation as registry referrers for the same digest.
7. Writes release evidence containing the service, image, digest, tag, source
   SHA, platform, `Containerfile` hash, aggregate migration hash, SBOM format,
   scan gate, signature method, and provenance type.
8. Uploads Buildx metadata, SPDX JSON, Trivy SARIF, and release JSON as a GitHub
   artifact with 90-day retention.

For a successful service artifact, expect `build-metadata.json`,
`<service>.spdx.json`, `<service>-trivy.sarif`, and
`<service>-release.json` under the uploaded `artifacts/` directory. A missing
file or a service artifact from a failed job is an evidence failure, even if the
image digest is present in GHCR.

The workflow pushes the image before scanning and signing it. A failed job can
therefore leave an unapproved image or incomplete referrers in GHCR. Presence in
GHCR, or possession of a `v*` tag, is never sufficient approval. The workflow
also does not run `make release-check`; required CI on the tagged commit is an
external release precondition.

## Verification and evidence

Approve a candidate only after all six jobs have completed successfully. For
each service, reconcile these values rather than trusting a tag lookup:

- Workflow run ID and URL, environment approval, actor, protected tag, and
  tagged source SHA.
- GHCR repository and exact image digest.
- `source_sha`, `version`, image, digest, platform, `containerfile_sha256`, and
  `migrations_sha256` in the service release JSON.
- Buildx metadata digest, SPDX subject, Trivy target, Cosign signature subject,
  and both GitHub attestation subjects.
- Successful scan conclusion and any accepted findings below the enforced
  severity threshold.

Use the exact digest from release evidence. The following checks illustrate the
expected identities; use organization-pinned versions of `cosign` and `gh` in
the verification environment:

```bash
TAG=v1.2.3
IMAGE=ghcr.io/assumeengagetry/distributed-commerce/user-service
DIGEST=sha256:<64-lowercase-hex-characters>
REF="$IMAGE@$DIGEST"

cosign verify \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/assumeengagetry/Distirbuted_Commerce/.github/workflows/release.yml@refs/tags/$TAG" \
  "$REF"

gh attestation verify "oci://$REF" \
  --repo assumeengagetry/Distirbuted_Commerce \
  --predicate-type https://slsa.dev/provenance/v1

gh attestation verify "oci://$REF" \
  --repo assumeengagetry/Distirbuted_Commerce \
  --predicate-type https://spdx.dev/Document/v2.3
```

Verification must fail if the subject digest, issuer, repository, workflow path,
tag, or predicate type differs. Private-repository verification may require an
authenticated client and different transparency behavior; validate that
contract before relying on it.

Create one release record containing all six service digests and evidence
artifact checksums, plus:

- Required CI and release workflow run IDs and conclusions.
- Tag ruleset and production-environment approval evidence.
- Release approvers, timestamp, change or incident ticket, and accepted risks.
- Source, overlay, admission-policy, and deployment-system revisions.
- External PostgreSQL backup identifier, PITR coverage time, restore-drill
  reference, and declared RPO/RTO. Do not put database credentials or backup
  decryption material in the record.
- Previous production digests, schema compatibility decision, migration result,
  rollout verification, and rollback deadline.

Copy the evidence bundle and run metadata into the external audit archive,
record checksums, and test retrieval. A GitHub artifact, registry referrer, or
repository document alone is not a complete long-term release record.

## Kyverno admission

The optional
[ImageValidatingPolicy](../security/policies/kyverno/verify-images.yaml) is a
cluster-scoped, fail-closed contract for Kyverno 1.18.2 or newer. It is not part
of the application Kustomize overlay. For matching project GHCR images on Pod
create or update, including generated Deployment and Job checks, it requires:

- resolution and verification of the image's signed digest;
- a Cosign keyless signature from this repository's trusted tag workflow;
- GitHub SLSA v1 provenance; and
- a signed SPDX 2.3 SBOM attestation.

The checked-in policy uses `Deny`, `failurePolicy: Fail`, a 30-second webhook
timeout, digest mutation and verification, and no background evaluation. The
issuer, repository spelling, workflow path, tag pattern, registry namespace,
and public Rekor URL are deliberate trust inputs. Forks, renamed repositories,
private repositories, other registries, and other workflow paths must use a
separately reviewed identity contract.

Before enforcement, a platform administrator must:

1. Install a supported Kyverno version in high availability mode and monitor
   webhook health and latency.
2. Validate cluster access to GHCR, GitHub OIDC evidence, Fulcio, and Rekor,
   including private registry credentials where applicable.
3. Review the policy and test an audit variant in a non-production namespace
   with a valid explicit digest, a valid signed tag to observe digest mutation,
   and unsigned, wrongly signed, unattested, and unavailable-registry cases.
4. Apply the reviewed deny policy separately and prove that each approved
   service digest is admitted, every negative case is denied, and tag mutation
   behaves as expected for the installed Kyverno version.
5. Define an audited break-glass process. Do not change the policy to ignore an
   outage during an ordinary release.

Digest mutation in admission is defense in depth. It does not permit a mutable
tag in a promoted overlay; the manifest verifier still requires explicit
digests. Because background evaluation is disabled, separately audit existing
workloads after policy installation or policy changes.

## Overlay promotion

The checked-in
[production overlay](../deploy/kubernetes/overlays/production/kustomization.yaml)
is a template with invalid registries and zero digests. It is not a deployable
environment and the release workflow does not edit it.

Promote a release through a reviewed environment or GitOps change:

1. Select the five workload digests from one approved release record. Do not
   resolve them from mutable tags and do not combine services from different
   runs without a documented compatibility decision.
2. Exercise those exact digests in a pre-production environment with the same
   admission identity and migration ordering. Environment-specific overlays or
   delivery repositories remain external unless explicitly added and reviewed.
3. Replace only the five image names and digests in the target production
   overlay. Keep Secrets and provider-specific endpoints in the external
   delivery and secret systems.
4. Render and validate the candidate:

```bash
make kube-check
bin/kustomize-v5.8.1 build deploy/kubernetes/overlays/production \
  > /tmp/commerce-production.yaml
make release-manifest-check FILE=/tmp/commerce-production.yaml
```

5. Verify signatures and both attestations for every rendered digest, then
   submit the overlay change for application, database, security, and operations
   review as required by the change policy.
6. Record the approved overlay revision and deploy through the
   [migration-first rollout](operations.md#migration-first-rollout). Confirm the
   migrator and all four resident workload rollouts before normal traffic.
7. Attach deployment, admission, migration, smoke-test, and monitoring results
   to the release record.

`release-manifest-check` rejects Secret resources, images outside the configured
project registry, mutable references, known placeholder hosts or tags, duplicate
image references, and a manifest with fewer than five application images. It does
not independently recognize an all-zero digest, verify registry existence,
signatures, attestations, vulnerability results, dependency endpoints, schema
compatibility, or cluster admission. Reject zero digests by review and release
evidence comparison before promotion; those remain separate release gates.

## Failure and rollback

### Publication failure

- Treat any failed or cancelled matrix job as a failed candidate. Do not promote
  a subset of its images.
- A Trivy failure occurs after the image was pushed. A signing, attestation, or
  artifact failure can also leave partial registry state. Mark those digests
  unapproved and preserve the run logs. Delete registry content only under the
  retention and incident policy; deletion is not a substitute for evidence.
- A workflow rerun is a new candidate evaluation. Reconcile and approve its
  exact digests because a rebuild need not reproduce a prior digest. Never move
  the protected tag to different source. Prefer a new version tag after a source
  correction.
- Do not bypass failed scans or missing Sigstore/GitHub evidence because an
  external service is unavailable. Record the outage and retry or issue a new
  candidate after service recovery.

### Admission failure

Inspect the admission response and Kyverno events for the exact digest,
registry authentication, issuer, workflow subject, predicate type, Rekor
connectivity, and webhook health. Compare the registry referrers with release
evidence. Do not weaken `Deny`, `failurePolicy: Fail`, or the trusted identity as
an unreviewed workaround. Reconcile the previous approved overlay if production
availability is affected.

### Deployment or migration failure

Stop promotion and preserve the release record, rendered manifest, cluster
events, admission decision, migration logs, image digests, and request IDs. If
the schema remains compatible, restore the previous approved digests in the
declarative overlay and reconcile. A direct `kubectl rollout undo` is only a
temporary incident action if GitOps still declares the failed digest.

Do not run an old binary against a schema until compatibility is established.
Do not use a down migration as routine rollback. A failed or dirty migration
follows the controlled procedure in
[backup-restore.md](backup-restore.md#migration-dirty-state). Restoring
PostgreSQL is an external provider operation with an explicit recovery point,
data-loss decision, and human approval.

After rollback, verify identity RPC compatibility, readiness, controlled order
and payment idempotency, worker health, and queue state. Keep the failed image
and evidence available for investigation, revoke or delete it only under the
incident policy, and record the final production digests and database version.

## Responsibility boundary

Phase 9 implements the release workflow, evidence generation, manifest verifier,
and optional Kyverno policy file. The organization and platform operators still
own tag and branch rulesets, environment approval, Actions policy, GHCR access
and retention, long-term evidence custody, Kyverno lifecycle, registry and
Sigstore availability, overlay review, deployment, external dependencies,
backup, and disaster recovery. Passing the workflow and admission policy does
not prove that an environment is recoverable or vulnerability-free.
