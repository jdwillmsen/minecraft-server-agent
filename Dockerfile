FROM golang:1.27-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b AS build
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
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/agent /agent

VOLUME ["/data"]
ENTRYPOINT ["/agent"]
