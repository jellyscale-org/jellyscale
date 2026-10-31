FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /jellyscale ./cmd/jellyscale

FROM alpine:3.24
RUN apk add --no-cache ffmpeg font-dejavu
COPY --from=build /jellyscale /usr/local/bin/jellyscale
USER 1000:1000
ENTRYPOINT ["jellyscale"]
