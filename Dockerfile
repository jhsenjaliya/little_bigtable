ARG GO_BASE_IMAGE=public.ecr.aws/docker/library/golang:1.25-alpine
ARG RUNTIME_BASE_IMAGE=public.ecr.aws/docker/library/alpine:3.22
ARG GO_OFFLINE=false
ARG GO_OFFLINE_DEPS_DIR=.docker/offline-go

FROM ${GO_BASE_IMAGE} AS build

WORKDIR /src
ENV GODEBUG=http2client=0
ARG GO_OFFLINE
ARG GO_OFFLINE_DEPS_DIR
RUN sed -i 's/https:/http:/' /etc/apk/repositories \
    && apk add --no-cache gcc musl-dev
RUN --mount=type=secret,id=ca_bundle,target=/tmp/ca-bundle.pem,required=false \
    if [ -s /tmp/ca-bundle.pem ]; then cat /tmp/ca-bundle.pem >> /etc/ssl/certs/ca-certificates.crt; fi
COPY ${GO_OFFLINE_DEPS_DIR}/ /offline-go/
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    if [ "$GO_OFFLINE" = "true" ]; then \
        if [ -d /offline-go/vendor ]; then \
            mkdir -p /src/vendor && cp -a /offline-go/vendor/. /src/vendor/; \
        elif [ -d /offline-go/mod ]; then \
            cp -a /offline-go/mod/. /go/pkg/mod/; \
            GOPROXY=off GOSUMDB=off go mod download; \
        else \
            echo "GO_OFFLINE=true requires ${GO_OFFLINE_DEPS_DIR}/vendor or ${GO_OFFLINE_DEPS_DIR}/mod in the Docker build context" >&2; \
            exit 1; \
        fi; \
    else \
        for attempt in 1 2 3 4 5; do \
            go mod download && break; \
            if [ "$attempt" = 5 ]; then exit 1; fi; \
            sleep "$((attempt * 5))"; \
        done; \
    fi
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    if [ "$GO_OFFLINE" = "true" ] && [ -d /src/vendor ]; then \
        CGO_ENABLED=1 GOPROXY=off GOSUMDB=off GOFLAGS=-mod=vendor go build -trimpath -ldflags="-s -w -linkmode external -extldflags -static" -o /out/bigtable-emulator-extended .; \
    elif [ "$GO_OFFLINE" = "true" ]; then \
        CGO_ENABLED=1 GOPROXY=off GOSUMDB=off GOFLAGS=-mod=mod go build -trimpath -ldflags="-s -w -linkmode external -extldflags -static" -o /out/bigtable-emulator-extended .; \
    else \
        CGO_ENABLED=1 go build -trimpath -ldflags="-s -w -linkmode external -extldflags -static" -o /out/bigtable-emulator-extended .; \
    fi

FROM ${RUNTIME_BASE_IMAGE}

COPY --from=build /out/bigtable-emulator-extended /usr/local/bin/bigtable-emulator-extended

ENTRYPOINT ["/usr/local/bin/bigtable-emulator-extended"]
