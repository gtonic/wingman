# syntax=docker/dockerfile:1

FROM golang:1-alpine AS build

# Install C build tools needed for CGo and go-sqlite3
RUN apk add --no-cache gcc musl-dev sqlite-dev

WORKDIR /src

COPY go.* ./
RUN go mod download

COPY . .
# Enable CGO for the server build, as go-sqlite3 requires it.
# Other binaries (client, ingest) can remain CGO_ENABLED=0 if they don't use CGo.
RUN CGO_ENABLED=1 go build -o /server /src/cmd/server
RUN CGO_ENABLED=0 go build -o /client /src/cmd/client
RUN CGO_ENABLED=0 go build -o /ingest /src/cmd/ingest


FROM alpine

# mailcap is for signal-cli, ca-certificates for HTTPS, tini as init
# No specific SQLite runtime libs needed here if go-sqlite3 statically links,
# which it should with CGO_ENABLED=1 and dev libs in build stage.
RUN apk add --no-cache tini ca-certificates mailcap

COPY --from=build /server /client /ingest /

EXPOSE 8080

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["/server"]
