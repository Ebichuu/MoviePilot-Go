# syntax=docker/dockerfile:1.7

FROM alpine:3.22 AS frontend
COPY deploy/frontend-release.env /tmp/frontend-release.env
RUN set -eu; \
    . /tmp/frontend-release.env; \
    apk add --no-cache curl unzip; \
    curl --fail --location --show-error \
      --connect-timeout 10 --max-time 180 \
      --retry 5 --retry-delay 2 --retry-all-errors \
      "https://github.com/jxxghp/MoviePilot-Frontend/releases/download/${FRONTEND_VERSION}/dist.zip" \
      --output /tmp/dist.zip; \
    echo "${FRONTEND_SHA256}  /tmp/dist.zip" | sha256sum -c -; \
    unzip -q /tmp/dist.zip -d /frontend; \
    test "$(tr -d '\r\n' < /frontend/dist/version.txt)" = "${FRONTEND_VERSION}"; \
    rm -f /frontend/dist/nginx.conf /tmp/dist.zip /tmp/frontend-release.env

FROM golang:1.26.3-alpine3.22 AS build
# 版本号由流水线注入（见 .github/workflows），本地直接 docker build 时是 dev。
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags="-s -w \
        -X github.com/moviepilot-go/moviepilot-go/internal/version.Version=${VERSION} \
        -X github.com/moviepilot-go/moviepilot-go/internal/version.Commit=${COMMIT} \
        -X github.com/moviepilot-go/moviepilot-go/internal/version.BuildDate=${BUILD_DATE}" \
      -o /out/moviepilot-go ./cmd/moviepilot-go

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/moviepilot-go /app/moviepilot-go
COPY --from=frontend /frontend/dist /app/web/dist
EXPOSE 3000
USER nonroot:nonroot
ENTRYPOINT ["/app/moviepilot-go"]
