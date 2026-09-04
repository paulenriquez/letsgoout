# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/letsgoout .

FROM alpine:3.23
RUN addgroup -S letsgoout && adduser -S -G letsgoout -u 10001 letsgoout \
    && mkdir -p /data && chown letsgoout:letsgoout /data
COPY --from=build /out/letsgoout /usr/local/bin/letsgoout
USER letsgoout
VOLUME ["/data"]
EXPOSE 8080
ENV LISTEN_ADDRESS=0.0.0.0:8080 DATABASE_PATH=/data/letsgoout.db
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/letsgoout"]
