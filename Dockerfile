# Build a static binary, then ship it on a distroless base.
#
# CGO is off because the SQLite driver is pure Go (modernc.org/sqlite), and the tz
# database is embedded in the binary via time/tzdata — so the runtime image needs
# nothing but CA certificates for TLS to Gmail and DiscGolfScene.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first so the module layer caches independently of source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:latest

# Runs as root so the mounted Fly volume at /data is writable.
COPY --from=build /out/server /usr/local/bin/server

ENV PORT=8080 \
    DB_PATH=/data/badges.db

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/server"]
