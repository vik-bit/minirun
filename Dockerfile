# Image for the minicni DaemonSet. The daemon copies this same binary onto each
# node, where the kubelet runs it as the CNI plugin, so it must be static.
FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /minicni ./cmd/minicni

FROM scratch
COPY --from=build /minicni /minicni
ENTRYPOINT ["/minicni", "daemon"]
