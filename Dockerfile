FROM --platform=$BUILDPLATFORM golang:1.25.13-bookworm@sha256:e401dae1bf814e29204a8cb7915682e1780951e609ca0dd8865ee1937f510c48 AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false -o /out/layercache ./cmd/layercache
RUN mkdir -p /out/state /out/config

# Team Cache API image. KVM workers run on dedicated native hosts separately.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/layercache /usr/local/bin/layercache
COPY --from=build --chown=65532:65532 /out/state /var/lib/layercache
COPY --from=build --chown=65532:65532 /out/config /etc/layercache
USER 65532:65532
EXPOSE 7437
ENTRYPOINT ["/usr/local/bin/layercache"]
CMD ["serve", "--config", "/etc/layercache/config.json"]
