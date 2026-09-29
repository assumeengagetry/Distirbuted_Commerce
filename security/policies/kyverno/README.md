# Kyverno image admission

`verify-images.yaml` is an optional cluster-level, fail-closed admission contract for Kyverno 1.18.2 or newer. It permits only images under the project GHCR namespace and requires all three registry referrers produced by the trusted tag workflow:

- a Cosign keyless image signature;
- GitHub SLSA v1 build provenance;
- a signed SPDX 2.3 SBOM attestation.

Install Kyverno in high availability mode and validate registry, Fulcio, Rekor, and GitHub OIDC connectivity before enforcing this policy. Applying a fail-closed policy without those dependencies can block Deployment and Job creation. Private repositories or a different workflow path use a different signing identity and must update the issuer/subject contract deliberately.

The policy is not part of the application Kustomize overlay because it is cluster-scoped and requires the Kyverno CRDs. A platform administrator should render, review, test in audit mode, and then apply it separately.
