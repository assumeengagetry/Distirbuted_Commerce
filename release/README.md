# Release evidence

The trusted tag workflow writes per-service release evidence as workflow
artifacts. This directory intentionally contains no live registry digest or
credential. A delivery repository may copy reviewed, signed evidence here and
then update the production Kustomize overlay in the same change.

Each evidence record includes the source SHA, image digest, platform,
Containerfile hash, aggregate migration hash, SBOM/scan/signature/provenance
methods, and the workflow run identity. Verify the record against the registry
and OCI referrers before promotion; do not treat a mutable tag or a downloaded
artifact checksum as a deployment identity.
