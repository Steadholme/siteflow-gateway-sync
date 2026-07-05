# syntax=docker/dockerfile:1

# ---- build stage: static, CGO-free binary -------------------------------------
FROM golang:1.26-alpine AS build
WORKDIR /src

# ca-certificates are copied into the scratch runtime so outbound TLS to Postgres
# (sslmode=require/verify-*) works.
RUN apk add --no-cache ca-certificates

# Cache module downloads on an unchanged go.mod/go.sum.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Fully static binary so it runs on scratch. -trimpath + -s -w shrink it.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/siteflow-gateway-sync ./cmd/siteflow-gateway-sync

# ---- runtime stage: minimal, non-root scratch ---------------------------------
FROM scratch
WORKDIR /app

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/siteflow-gateway-sync /usr/local/bin/siteflow-gateway-sync

# Non-root numeric UID/GID; scratch has no /etc/passwd so a name is not usable.
USER 65532:65532

# Bind on all interfaces inside the container; the healthcheck probes loopback.
ENV BIND_ADDR=0.0.0.0:9385
EXPOSE 9385

# Self-probe /healthz with the built-in flag — no shell or wget needed on scratch.
HEALTHCHECK --interval=10s --timeout=3s --start-period=3s --retries=3 \
    CMD ["/usr/local/bin/siteflow-gateway-sync", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/siteflow-gateway-sync"]
