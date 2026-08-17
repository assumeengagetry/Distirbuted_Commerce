# Production overlay

This overlay is a reviewed deployment contract, not a ready-to-apply environment. Before rendering it for a cluster:

1. Replace every `registry.example.invalid` image and zero digest in `kustomization.yaml` with an immutable image produced from the repository `Containerfile`.
2. Replace the PostgreSQL and queue Redis hostnames in the base ConfigMaps. Keep cache Redis disabled until a separately isolated cache endpoint is provisioned.
3. Create every Secret described in `secrets-contract.md` through an external secret controller or an audited deployment system. Do not commit rendered Secret data.
4. Issue HTTP certificates for the externally routed service names. The ingress or Gateway must use TLS passthrough or verified HTTPS upstream connections.
5. Issue the user gRPC server certificate for `commerce-user.commerce.svc.cluster.local`. Issue order/payment client certificates with exactly one URI SAN matching the configured SPIFFE allowlist.
6. Label the trusted ingress namespace with `commerce.network/ingress=true`. Add environment-specific egress policy using real dependency CIDRs or an FQDN-aware CNI.
7. Run and wait for `commerce-migrate` before assessing API readiness. Database runtime and migrator roles should be distinct.

Metrics bind to Pod loopback by design. A production metrics pipeline needs a same-Pod Prometheus/Collector sidecar or another trusted loopback scraper; a `ServiceMonitor` cannot directly reach these listeners. Trace export is disabled in the base. Enable it only after mounting an explicit OTLP CA/mTLS contract and setting an HTTPS root OTLP/gRPC endpoint.

Certificates and the PASETO key are loaded only at process startup. Rotate file Secrets with a rollout. The symmetric PASETO key requires a coordinated cutover because there is no multi-key verification window.
