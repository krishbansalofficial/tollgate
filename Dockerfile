# syntax=docker/dockerfile:1
# Build on the runner's native arch and cross-compile (BUILDPLATFORM), which is
# far faster than building arm64 under QEMU emulation. Pure Go, so CGO off.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/ ./cmd/tollgate ./cmd/mockupstream

# distroless/static: no shell, no package manager, ~2 MB; :nonroot runs as UID 65532.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/ /usr/local/bin/
COPY config/catalog.yaml /etc/tollgate/catalog.yaml
ENV TOLLGATE_CATALOG=/etc/tollgate/catalog.yaml
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/tollgate"]
CMD ["serve"]
