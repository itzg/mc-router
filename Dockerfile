FROM golang:1.25 AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false ./cmd/mc-router

FROM alpine AS certs
RUN apk add -U \
    ca-certificates

FROM scratch
ENTRYPOINT ["/mc-router"]
COPY --from=certs /etc/ssl/certs/ /etc/ssl/certs
COPY --from=builder /build/mc-router /mc-router
