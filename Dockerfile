# syntax=docker/dockerfile:1.7

FROM golang:1.26-alpine AS build
WORKDIR /src

# Dependencies first so source edits do not invalidate the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/gateway ./cmd/gateway && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w" \
    -o /out/mockupstream ./cmd/mockupstream && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w" \
    -o /out/token ./cmd/token

FROM alpine:3.22 AS runtime
RUN apk add --no-cache ca-certificates wget && \
    addgroup -S gateway && adduser -S -G gateway gateway
WORKDIR /app

COPY --from=build /out/gateway /app/gateway
COPY --from=build /out/mockupstream /app/mockupstream
COPY --from=build /out/token /app/token
COPY configs /app/configs

USER gateway
EXPOSE 8080 9090 9100
ENTRYPOINT ["/app/gateway"]
CMD ["-config", "/app/configs/gateway.compose.yaml"]
