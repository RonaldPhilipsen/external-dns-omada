# syntax=docker/dockerfile:1

# Build the webhook provider binary from source. golang:1.27 because
# sigs.k8s.io/external-dns v0.23.0 requires go >= 1.27.
FROM golang:1.27 AS build
ARG VERSION=devel
ARG GIT_SHA=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.BuildVersion=${VERSION} -X main.BuildCommitSha=${GIT_SHA}" \
    -o /out/external-dns-omada .

# Static distroless runtime: no shell, non-root user.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/external-dns-omada /usr/local/bin/external-dns-omada
EXPOSE 8080 8888
ENTRYPOINT ["/usr/local/bin/external-dns-omada"]
