FROM golang:1.26.5 AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 go build -buildvcs=false ./cmd/mc-router

FROM cgr.dev/chainguard/static
ENTRYPOINT ["/mc-router"]
COPY --from=builder /build/mc-router /mc-router
