# syntax=docker/dockerfile:1

# The Fleetling image: the static fleetling binary plus the docker CLI, the
# Compose plugin and buildx, which it shells out to. kompose and podlet join
# with the converter in phase 10.

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
COPY --from=build /out/fleetling /usr/local/bin/fleetling
ENV FLEETLING_ROOT=/opt
EXPOSE 8420
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/fleetling", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/fleetling"]
