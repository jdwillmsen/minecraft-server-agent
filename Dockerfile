FROM golang:1.27-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS build
WORKDIR /src

# The contract module is a local replace target, so go mod download needs
# its go.mod before the rest of the source arrives.
COPY go.mod go.sum ./
COPY presenceapi/go.mod presenceapi/
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/agent ./cmd/agent
# One codebase, one image: census runs as a scheduled job beside the
# server rather than inside the agent process, so its binary rides along
# here instead of getting a Dockerfile of its own.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/census ./cmd/census
# Same reasoning as census: the probe speaks the same Bedrock handshake this
# module already implements, so it rides here rather than growing a second
# image to pin, pull and keep in step with the protocol code it shares.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/joinprobe ./cmd/joinprobe

# gophertunnel's RakNet implementation is pure Go - no cgo, no native
# addon - so the only runtime requirement is the binary and TLS roots for
# the Xbox Live device-code login. distroless/static bundles CA certs and a
# non-root user without a shell, which a bare `FROM scratch` cannot offer
# without vendoring the cert bundle by hand.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/agent /agent
COPY --from=build /out/census /census
COPY --from=build /out/joinprobe /joinprobe

# Redundant at runtime -- the `:nonroot` base already runs as uid 65532 -- but
# a scanner reading this file cannot resolve a digest-pinned base image's USER,
# so without the line it reports the image as running root. Stating it makes
# the property checkable from the Dockerfile alone, and survives a future base
# image change that quietly reverts to root.
USER nonroot:nonroot

VOLUME ["/data"]
ENTRYPOINT ["/agent"]
