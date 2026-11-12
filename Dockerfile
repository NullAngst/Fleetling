# syntax=docker/dockerfile:1

# The Fleetling image: the static fleetling binary plus the tools it shells
# out to: the docker CLI with the Compose and buildx plugins, kompose and
# podlet. Every tool is the latest release, verified before it goes in.

# Stage 1: bundle the browser code. Node only exists in this stage.
FROM node:lts-alpine AS assets
WORKDIR /src/web/src
COPY web/src/package.json web/src/package-lock.json ./
RUN npm ci --ignore-scripts
COPY web/src/ ./
RUN npm run build

# Stage 2: build the binary.
FROM golang:alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY . .
COPY --from=assets /src/web/static/dist ./web/static/dist
ARG VERSION=dev
# go.sum is committed by the deps workflow. If it is missing, go mod tidy
# writes it here and checks every module against sum.golang.org.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    if [ ! -f go.sum ]; then go mod tidy; fi && \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/fleetling ./cmd/fleetling

# kompose: `go install github.com/kubernetes/kompose@latest` doesn't work,
# because kompose's go.mod has replace directives, which go install refuses.
# So this takes the latest release binary and checks it against the
# SHA256_SUM file published with that release. That catches a corrupted or
# swapped download; it can't catch a compromised release, since the sum
# comes from the same place.
FROM alpine:latest AS kompose
ARG TARGETARCH
RUN apk add --no-cache curl
RUN set -eu; \
    arch="${TARGETARCH:-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')}"; \
    latest=$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/kubernetes/kompose/releases/latest); \
    tag="${latest##*/}"; \
    base="https://github.com/kubernetes/kompose/releases/download/${tag}"; \
    curl -fsSLo /kompose "${base}/kompose-linux-${arch}"; \
    curl -fsSLo /SHA256_SUM "${base}/SHA256_SUM"; \
    want=$(awk -v n="kompose-linux-${arch}" '$2 == n { print $1 }' /SHA256_SUM); \
    [ -n "$want" ] || { echo "no checksum for kompose-linux-${arch} in ${tag}" >&2; exit 1; }; \
    echo "${want}  /kompose" | sha256sum -c -; \
    chmod 755 /kompose; \
    /kompose version

# podlet: built from crates.io with the lock file it was published with.
# cargo checks every crate against the crates.io index checksums.
FROM rust:alpine AS podlet
RUN apk add --no-cache musl-dev
RUN cargo install --locked --root /out podlet && /out/bin/podlet --version

# The official docker:cli image downloads the CLI, Compose and buildx and
# checks each one against its published sha256. Copying from it pulls the
# latest release, verified by image digest.
FROM docker:cli AS cli

# Stage 3: the runtime image.
FROM alpine:latest
RUN apk add --no-cache ca-certificates tzdata
COPY --from=cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=cli /usr/local/libexec/docker/cli-plugins/docker-compose /usr/local/libexec/docker/cli-plugins/docker-compose
COPY --from=cli /usr/local/libexec/docker/cli-plugins/docker-buildx /usr/local/libexec/docker/cli-plugins/docker-buildx
COPY --from=kompose /kompose /usr/local/bin/kompose
COPY --from=podlet /out/bin/podlet /usr/local/bin/podlet
COPY --from=build /out/fleetling /usr/local/bin/fleetling
ENV FLEETLING_ROOT=/opt
EXPOSE 8420
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/fleetling", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/fleetling"]
