FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
COPY deploy/fixtures/seed deploy/fixtures/seed
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /jellyscale ./cmd/jellyscale
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /seed ./deploy/fixtures/seed

# What the server and the fixtures image share: the binary, ffmpeg, the user and the labels.
# A build that names no target gives the last stage, server.
FROM alpine:3.24 AS base
RUN apk add --no-cache ffmpeg font-dejavu
COPY --from=build /jellyscale /usr/local/bin/jellyscale
ARG REVISION=
ARG VERSION=
LABEL org.opencontainers.image.revision=$REVISION \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.source=https://github.com/jellyscale-org/jellyscale
USER 1000:1000
ENTRYPOINT ["jellyscale"]

# The server image plus a small synthetic library, for a client's tests (docs/client-testing.md). test/image.sh
# builds it from a context that holds media/, the films of test/fixtures/client.sh; the config is deploy/fixtures/config.json.
# Nothing here makes a film, and no password is here: the compose file sets it. seed copies some of the films into the
# S3 library's bucket when the compose project starts (deploy/fixtures/seed).
FROM base AS fixtures
COPY --from=build /seed /usr/local/bin/seed
COPY --chown=1000:1000 deploy/fixtures/config.json /fixtures/config.json
COPY --chown=1000:1000 media /fixtures/media
CMD ["--roles=all", "--config=/fixtures/config.json"]

FROM base AS server
