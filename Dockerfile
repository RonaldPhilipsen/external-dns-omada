# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e

# Build the webhook provider binary from source. golang:1.27 because
# sigs.k8s.io/external-dns v0.23.0 requires go >= 1.27.
FROM golang:1.27@sha256:6928d94d1142af58816fed5a62338d605a266ccd53a0deaab6b1096f19059261 AS build
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
FROM gcr.io/distroless/static:nonroot@sha256:516757106de35205efb70b91ba03f6f1298424a0678c603255cf2852a7000087
COPY --from=build /out/external-dns-omada /usr/local/bin/external-dns-omada
EXPOSE 8080 8888
ENTRYPOINT ["/usr/local/bin/external-dns-omada"]
