# Release artifacts

Cloud Harness releases provide Go binaries and a container image.

## Binaries

Build the current platform binary:

```bash
go build -o cloud-harness ./cmd/cloud-harness
```

Cross-build release targets with `CGO_ENABLED=0` because SQLite is pure Go:

```bash
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
  goos=${target%/*}; goarch=${target#*/}; ext=""
  if [ "$goos" = windows ]; then ext=".exe"; fi
  GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=$(git describe --tags --always)+$(git rev-parse --short HEAD)" \
    -o "dist/cloud-harness_${goos}_${goarch}${ext}" ./cmd/cloud-harness
done
```

The GitHub `release` workflow runs on `v*` tags, builds archives for Linux, macOS, and Windows, writes `checksums.txt`, and attaches the archives to the GitHub release.

## Container image

Build locally:

```bash
docker build -t cloud-harness .
```

Run the HTTP server with a workspace and persistent data directory mounted:

```bash
docker run --rm -p 19421:19421 \
  -v "$PWD:/workspace" \
  -v cloud-harness-data:/data \
  -e CLOUD_HARNESS_PROVIDER=openai \
  -e OPENAI_API_KEY=sk-... \
  -e OPENAI_MODEL=gpt-4.1-mini \
  cloud-harness
```

The image defaults to `mock` provider, listens on `0.0.0.0:19421`, stores SQLite in `/data/harness.db`, and serves the embedded Web UI. Override command arguments after the image name when needed, for example `docker run cloud-harness version`.

The same `release` workflow builds and pushes `ghcr.io/brianogg/cloud-harness` for `linux/amd64` and `linux/arm64` on `v*` tags, with semver, major.minor, sha, and latest tags.
