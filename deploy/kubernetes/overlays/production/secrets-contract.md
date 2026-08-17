# Secret contract

The manifests reference these externally managed Secrets in namespace `commerce`:

| Secret | Required keys | Consumer |
| --- | --- | --- |
| `commerce-runtime` | `database-url-user`, `database-url-order`, `database-url-payment`, `database-url-worker`, `database-url-migrator`, `paseto-v4-local-key`, `queue-redis-password` | all processes |
| `commerce-postgres-ca` | `ca.crt` | all database clients |
| `commerce-queue-redis-ca` | `ca.crt` | user and worker |
| `commerce-user-http-tls` | `tls.crt`, `tls.key` | user HTTPS |
| `commerce-order-http-tls` | `tls.crt`, `tls.key` | order HTTPS |
| `commerce-payment-http-tls` | `tls.crt`, `tls.key` | payment HTTPS |
| `commerce-user-grpc-tls` | `tls.crt`, `tls.key` | identity gRPC server |
| `commerce-order-grpc-tls` | `tls.crt`, `tls.key` | order identity client |
| `commerce-payment-grpc-tls` | `tls.crt`, `tls.key` | payment identity client |
| `commerce-identity-ca` | `ca.crt` | identity server/client trust |

Every database URL must use `sslmode=verify-full` and reference `/var/run/secrets/postgres/ca.crt`. The migrator URL belongs to a schema-owner role; runtime URLs should use least-privilege roles. Raw URLs, passwords, PASETO keys, private keys, and OTLP authorization headers must not appear in ConfigMaps, Kustomize output, CI artifacts, or logs.

If trace export is enabled, add a projected Secret containing the OTLP CA and optional client certificate/key, mount it read-only, and configure `OTEL_EXPORTER_OTLP_CERTIFICATE`, `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE`, and `OTEL_EXPORTER_OTLP_CLIENT_KEY`. Supply `OTEL_EXPORTER_OTLP_HEADERS` through a Secret-backed environment variable.
