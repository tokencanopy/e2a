# Pin the builder to the host's native arch (BUILDPLATFORM) so multi-arch
# `docker buildx build` doesn't re-run the Go compile under QEMU for each
# target. With CGO_ENABLED=0, Go cross-compiles cleanly to any platform
# from any host — TARGETOS/TARGETARCH pick the output's arch. The runtime
# stage below uses the default platform (set per-target by buildx) so the
# alpine layer matches the binary.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder
ARG TARGETOS TARGETARCH

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /e2a ./cmd/e2a

FROM alpine:3.24
# Capability marker: this image speaks PROXY protocol v1/v2 on the SMTP
# listener (smtp.proxy_trusted_cidrs). Deploy automation (e2a-ops) refuses to
# put a release without this label behind HAProxy's send-proxy-v2.
LABEL org.e2a.smtp-proxy="v1"
RUN apk add --no-cache ca-certificates wget \
    && adduser -D -u 1001 -h /home/e2a e2a
COPY --from=builder /e2a /usr/local/bin/e2a
COPY config.example.yaml /etc/e2a/config.yaml
USER e2a
EXPOSE 2525 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=5 \
    CMD wget -qO- http://127.0.0.1:8080/api/health || exit 1
ENTRYPOINT ["e2a", "-config", "/etc/e2a/config.yaml"]
