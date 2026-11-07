# syntax=docker/dockerfile:1

FROM golang:1.27.1-bookworm AS build

WORKDIR /src

ENV CGO_ENABLED=0
ENV GOOS=linux

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

RUN go build -mod=readonly -trimpath -o /out/migrate ./cmd/migrate \
    && go build -mod=readonly -trimpath -o /out/seed ./cmd/seed

FROM debian:bookworm-slim AS runtime

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=build --chown=10001:10001 /out/migrate /app/migrate
COPY --from=build --chown=10001:10001 /out/seed /app/seed

USER 10001:10001

CMD ["/app/migrate"]