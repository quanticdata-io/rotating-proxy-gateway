# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/quanticdata-io/rotating-proxy-gateway/internal/gateway.Version=${VERSION}" -o /out/rotating-proxy-gateway ./cmd/rotating-proxy-gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/rotating-proxy-gateway /usr/local/bin/rotating-proxy-gateway
EXPOSE 8080 8081
# Inside a container the ports must listen on all interfaces; publish them on
# 127.0.0.1 only (docker run -p 127.0.0.1:8080:8080) or set GATEWAY_AUTH.
ENTRYPOINT ["rotating-proxy-gateway", "--listen", "0.0.0.0:8080", "--admin", "0.0.0.0:8081"]
