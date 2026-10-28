# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e

# Build the webhook provider binary from source. golang:1.27 because
# sigs.k8s.io/external-dns v0.23.0 requires go >= 1.27.
FROM golang:1.27@sha256:2920bb124f1a2e12a1d39f0cab756ae0068441ee1da7655dec7f5e374944095b AS build
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
FROM gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
COPY --from=build /out/external-dns-omada /usr/local/bin/external-dns-omada
EXPOSE 8080 8888
ENTRYPOINT ["/usr/local/bin/external-dns-omada"]
