# Alpine version for the runner contianer
ARG ALPINE_VERSION=latest

# golang builder image
FROM golang:1.21-bullseye AS builder
WORKDIR /build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -pgo=auto -ldflags="-w -s -X 'main.Version=$(git describe --tags)'" -o app . \
	&& git clone https://github.com/andybalholm/redwood-config.git /tmp/redwood-config

# Final runner image
FROM alpine:${ALPINE_VERSION} AS runner

# Copy in config & app binary
COPY --from=builder /build/app /app
COPY --from=builder /tmp/redwood-config /etc/redwood
RUN mkdir -p /var/log/redwood

# Expose the default ports from redwood-config
EXPOSE 6502 6510

# Run the app
ENTRYPOINT [ "/app" ]