# Multi-stage: the toolchain never ships to production.
FROM golang:1.22-alpine AS build

WORKDIR /src

# Dependencies are copied and downloaded before the source so that editing code
# does not invalidate the module cache layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none

# CGO off produces a static binary that runs on a distroless base.
# -trimpath keeps build machine paths out of the binary.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=build /out/server /app/server
COPY --from=build /src/web /app/web

# Run unprivileged: a container compromise should not also be root.
USER nonroot:nonroot

EXPOSE 8080
ENV ADDR=:8080 \
    WEB_DIR=/app/web \
    LOG_FORMAT=json

ENTRYPOINT ["/app/server"]
