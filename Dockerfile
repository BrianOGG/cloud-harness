# syntax=docker/dockerfile:1

FROM golang:1.27.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}+${COMMIT}" -o /out/cloud-harness ./cmd/cloud-harness

FROM alpine:3.21
RUN apk add --no-cache \
    ca-certificates git bash coreutils findutils grep sed gawk curl jq make gcc musl-dev \
    python3 py3-pip nodejs npm openssh-client tar gzip unzip xz ripgrep fd sqlite \
    postgresql-client tzdata \
  && adduser -D -u 10001 harness \
  && mkdir -p /data /workspace \
  && chown -R harness:harness /data /workspace
WORKDIR /workspace
COPY --from=build /out/cloud-harness /usr/local/bin/cloud-harness
USER harness
ENV CLOUD_HARNESS_ADDR=0.0.0.0:19421 \
    CLOUD_HARNESS_DB=/data/harness.db \
    CLOUD_HARNESS_PROVIDER=mock
EXPOSE 19421
ENTRYPOINT ["cloud-harness"]
CMD ["serve", "--addr", "0.0.0.0:19421", "--db", "/data/harness.db", "--workspace", "/workspace"]
