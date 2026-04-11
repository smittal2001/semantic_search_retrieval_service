# Multi-stage build. The same Dockerfile builds all four services.
# Usage: docker build --build-arg SERVICE=worker -t semantic-worker .
#
# Stage 1: build
# Stage 2: minimal runtime image (~10MB)

# ── Stage 1: build ────────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Download dependencies first (cached unless go.mod/go.sum change)
COPY go.mod go.sum ./
RUN go mod download

# Copy all source code
COPY . .

# Build argument selects which service binary to compile
ARG SERVICE=gateway
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-w -s" \
    -o /bin/service \
    ./cmd/${SERVICE}

# ── Stage 2: minimal runtime ──────────────────────────────────────────────────
FROM scratch

# Copy CA certificates for HTTPS calls (embed API)
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy the compiled binary
COPY --from=builder /bin/service /service

# Run as non-root user (UID 65534 = nobody)
USER 65534

EXPOSE 8080 9090

ENTRYPOINT ["/service"]
