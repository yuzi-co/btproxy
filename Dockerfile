# The binary is static, so the image holds nothing else. Building on the build platform makes the arm images a
# cross-compile rather than an emulated build.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags="-s -w ${VERSION:+-X main.version=$VERSION}" -o /btproxy ./cmd/btproxy

FROM scratch
COPY LICENSE /LICENSE
COPY --from=build /btproxy /btproxy
# godbus defaults to /var/run/dbus, and this image has no /var/run link to /run.
ENV DBUS_SYSTEM_BUS_ADDRESS=unix:path=/run/dbus/system_bus_socket
ENTRYPOINT ["/btproxy"]
