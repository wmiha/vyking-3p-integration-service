# syntax=docker/dockerfile:1

ARG GO_VERSION=1.27.4
ARG ALPINE_VERSION=3.24

# ---- build-base: module cache + sources --------------------------------------
FROM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS build-base
WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-trimpath

# Dependencies first so source changes reuse the module cache layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .

# ---- build: the service binary -----------------------------------------------
FROM build-base AS build
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/server ./cmd/server

# ---- build-acmemock: the Acme Support stand-in (local stack only) -------------
FROM build-base AS build-acmemock
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w" -o /out/acmemock ./scripts/acmemock

# ---- acmemock: runtime image of the stand-in ---------------------------------
FROM gcr.io/distroless/static-debian13:nonroot AS acmemock
COPY --from=build-acmemock /out/acmemock /acmemock
EXPOSE 8081
USER nonroot:nonroot
ENTRYPOINT ["/acmemock"]

# ---- server: minimal runtime image (default target, keep it last) -----------
# distroless/static has no shell or package manager: only the binary, CA
# certificates and tzdata, running as an unprivileged user.
FROM gcr.io/distroless/static-debian13:nonroot AS server
WORKDIR /app
COPY --from=build /out/server /app/server
COPY config/ /app/config/
# Safe default: prod config requires every secret to be injected explicitly.
ENV APP_ENV=prod
EXPOSE 8080 9090
USER nonroot:nonroot
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 CMD ["/app/server", "healthcheck"]
ENTRYPOINT ["/app/server"]
