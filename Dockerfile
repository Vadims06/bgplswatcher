# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app
# Copy gobgp root go.mod for replace directive
COPY go.mod go.sum ./
COPY api ./api
COPY pkg ./pkg
COPY proto ./proto
COPY internal ./internal
# Copy only bgplswatcher source files
COPY cmd/bgplswatcher/go.mod cmd/bgplswatcher/go.sum ./cmd/bgplswatcher/
COPY cmd/bgplswatcher/main.go ./cmd/bgplswatcher/
COPY cmd/bgplswatcher/config.toml ./cmd/bgplswatcher/
# Copy gobgp CLI source files
COPY cmd/gobgp ./cmd/gobgp

WORKDIR /app/cmd/bgplswatcher

# Install build dependencies
RUN apk add --no-cache git

# Download dependencies
RUN go mod download

# Build bgplswatcher
RUN CGO_ENABLED=0 GOOS=linux go build -o bgplswatcher

# Build gobgp CLI
WORKDIR /app/cmd/gobgp
RUN CGO_ENABLED=0 GOOS=linux go build -o gobgp

# Final stage
FROM alpine:3.18

WORKDIR /app

# Copy the binary and config from builder
COPY --from=builder /app/cmd/bgplswatcher/bgplswatcher .
COPY --from=builder /app/cmd/bgplswatcher/config.toml .
# Copy gobgp CLI to /usr/local/bin so it's in PATH
COPY --from=builder /app/cmd/gobgp/gobgp /usr/local/bin/gobgp

# Create non-root user
RUN adduser -D -u 1000 gobgp && \
    chown -R gobgp:gobgp /app && \
    chown gobgp:gobgp /usr/local/bin/gobgp

USER gobgp

ENTRYPOINT ["/app/bgplswatcher"]
