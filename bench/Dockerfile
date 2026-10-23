# Benchmark image: iperf3 for throughput and rr for request/response latency.
FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY bench/rr/main.go .
RUN go mod init rr && CGO_ENABLED=0 go build -o /rr .

FROM alpine:3.20
RUN apk add --no-cache iperf3
COPY --from=build /rr /usr/local/bin/rr
