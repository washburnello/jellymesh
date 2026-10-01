# Jellymesh node image.
#
# The SQLite driver is pure Go, so the binary is built without cgo and runs on
# a distroless base with no shell. It runs as the distroless non-root user
# (65532), so the /data volume must be writable by that user.
FROM golang:1.27-alpine AS build
WORKDIR /src
# third_party holds the patched go-fuse the module replaces (#65).
COPY go.mod go.sum ./
COPY third_party ./third_party
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/jellymesh ./cmd/jellymesh
# An empty, owner-only data directory for the non-root user, so that a named
# volume mounted at /data starts out writable by it.
RUN mkdir -m 0700 /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/jellymesh /jellymesh
COPY --from=build --chown=65532:65532 /out/data /data
ENV JELLYMESH_DATA_DIR=/data \
    JELLYMESH_FEDERATION_LISTEN_ADDR=0.0.0.0:8443 \
    JELLYMESH_ADMIN_LISTEN_ADDR=127.0.0.1:8091
VOLUME ["/data"]
EXPOSE 8443
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/jellymesh", "healthcheck"]
ENTRYPOINT ["/jellymesh"]
CMD ["serve"]
