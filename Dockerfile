FROM --platform=$BUILDPLATFORM golang:1.16 as builder

WORKDIR /ethereum_exporter
COPY . .

# TARGETOS/TARGETARCH are set by buildx; empty with the classic builder,
# in which case go builds for the native platform as before.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=undefined
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags "-s -w -X main.version=$VERSION" github.com/31z4/ethereum-prometheus-exporter/cmd/ethereum_exporter

FROM scratch

ENTRYPOINT ["/ethereum_exporter"]
USER nobody
EXPOSE 9368

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /ethereum_exporter/ethereum_exporter /ethereum_exporter
