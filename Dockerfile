# syntax=docker/dockerfile:1
# Builds the tapyr image from source: the web UI, then the static binary,
# on a distroless base running as nonroot. Releases use Dockerfile.release
# with the binaries built by GoReleaser.

FROM --platform=$BUILDPLATFORM node:26-alpine@sha256:8996bb5821d62a8fd9bb27c1cb07462f3fdce6e47d640c6a881ead0b064ce216 AS ui
WORKDIR /src/ui
COPY ui/package.json ui/package-lock.json ./
COPY ui/scripts ./scripts
RUN npm ci --no-audit --no-fund
COPY ui/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS build
ARG TARGETOS TARGETARCH
ARG VERSION=0.0.0-dev
ARG REVISION=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/ui/dist/app ./ui/dist/app
COPY --from=ui /src/ui/dist/licenses.txt ./ui/dist/licenses.txt
RUN mkdir -p /out && go run ./internal/tools/licenses -ui ui/dist/licenses.txt -out /out/THIRD_PARTY_LICENSES
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/kentrow/tapyr/internal/buildinfo.version=${VERSION} -X github.com/kentrow/tapyr/internal/buildinfo.revision=${REVISION}" \
      -o /out/tapyr ./cmd/tapyr \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/tapyr /usr/local/bin/tapyr
COPY LICENSE NOTICE /usr/share/doc/tapyr/
COPY --from=build /out/THIRD_PARTY_LICENSES /usr/share/doc/tapyr/
# The state store defaults to ./data/tapyr.db: mount a volume on
# /home/nonroot/data to keep it. The directory belongs to nonroot, so that a
# new named volume does too.
COPY --from=build --chown=65532:65532 /out/data /home/nonroot/data
WORKDIR /home/nonroot
USER 65532:65532
EXPOSE 9790
ENTRYPOINT ["/usr/local/bin/tapyr"]
CMD ["run", "--config", "/etc/tapyr"]
