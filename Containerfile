# syntax=docker/dockerfile:1
ARG GO_IMAGE=docker.io/library/golang:1.26.6-alpine3.24@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83
FROM ${GO_IMAGE} AS build

ARG SERVICE
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY db ./db
COPY internal ./internal

RUN case "${SERVICE}" in \
      user-service|order-service|payment-service|job-worker|job-admin|migrator) ;; \
      *) printf 'unsupported SERVICE: %s\n' "${SERVICE}" >&2; exit 2 ;; \
    esac && \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
      go build -trimpath -buildvcs=false -ldflags='-s -w' -o /out/service "./cmd/${SERVICE}" && \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
      go build -trimpath -buildvcs=false -ldflags='-s -w' -o /out/loopback-probe ./cmd/loopback-probe

FROM scratch

ARG SERVICE
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="distributed-commerce-${SERVICE}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.source="https://github.com/assumeengagetry/Distirbuted_Commerce"

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=65532:65532 /out/service /app/service
COPY --from=build --chown=65532:65532 /out/loopback-probe /app/loopback-probe

USER 65532:65532
WORKDIR /app
STOPSIGNAL SIGTERM
ENTRYPOINT ["/app/service"]
