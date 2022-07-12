FROM golang:1.18 as builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build ./cmd/mc-router

FROM scratch
ENTRYPOINT ["/mc-router"]
COPY --from=builder /build/mc-router /mc-router
