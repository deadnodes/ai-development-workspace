FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /release-control ./cmd/server
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /rcp-runtime-reporter ./cmd/rcp-runtime-reporter
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /prod-signer ./cmd/prod-signer
FROM alpine:3.22
RUN apk add --no-cache ca-certificates git && adduser -D -u 10001 app && mkdir -p /data && chown app:app /data
USER app
COPY --from=build /release-control /usr/local/bin/release-control
COPY --from=build /rcp-runtime-reporter /usr/local/bin/rcp-runtime-reporter
COPY --from=build /prod-signer /usr/local/bin/prod-signer
ENV LISTEN_ADDR=:8080 RCP_DATA_PATH=/data/rcp.sqlite
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["release-control"]
