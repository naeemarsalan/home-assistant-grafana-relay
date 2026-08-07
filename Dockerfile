# Stage 1: Build Stage
FROM golang:1.19-alpine AS builder

WORKDIR /usr/app

# Install build dependencies if needed, and download modules
COPY go.mod ./
RUN go mod download && go mod verify

# Copy source code and build
COPY *.go ./
COPY web ./web
# -ldflags="-s -w" reduces binary size by stripping debug information
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -v -o /usr/local/bin/app ./...

# Stage 2: Final Runtime Stage
FROM alpine:3.18

# Add a non-root user and a writable location for persistent history
RUN adduser -D appuser && \
    mkdir -p /data && \
    chown appuser:appuser /data
USER appuser
WORKDIR /data

ENV HISTORY_FILE=/data/notifications.json \
    HISTORY_LIMIT=200
VOLUME ["/data"]

# Copy only the compiled binary from the builder stage
COPY --from=builder /usr/local/bin/app /usr/local/bin/app

CMD ["app"]
