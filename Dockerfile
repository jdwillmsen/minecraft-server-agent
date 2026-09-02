FROM golang:1.26-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/agent ./cmd/agent

# gophertunnel's RakNet implementation is pure Go - no cgo, no native
# addon - so the only runtime requirement is the binary and TLS roots for
# the Xbox Live device-code login. distroless/static bundles CA certs and a
# non-root user without a shell, which a bare `FROM scratch` cannot offer
# without vendoring the cert bundle by hand.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agent /agent

VOLUME ["/data"]
ENTRYPOINT ["/agent"]
