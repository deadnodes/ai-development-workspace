FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /release-control ./cmd/server
FROM alpine:3.22
RUN apk add --no-cache ca-certificates git && adduser -D -u 10001 app && mkdir -p /data && chown app:app /data
USER app
COPY --from=build /release-control /usr/local/bin/release-control
ENV LISTEN_ADDR=:8080 RCP_DATA_PATH=/data/state.db
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["release-control"]
