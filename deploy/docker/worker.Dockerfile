# syntax=docker/dockerfile:1

FROM golang:1.27.1-bookworm AS build

WORKDIR /src

ENV CGO_ENABLED=0
ENV GOOS=linux

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

RUN go build -mod=readonly -trimpath -o /out/worker ./cmd/worker

FROM debian:bookworm-slim AS runtime

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=build --chown=10001:10001 /out/worker /app/worker

COPY --chown=10001:10001 \
    deploy/elasticsearch/rule-chunks-v1.json \
    /app/deploy/elasticsearch/rule-chunks-v1.json

USER 10001:10001

STOPSIGNAL SIGTERM

ENTRYPOINT ["/app/worker"]