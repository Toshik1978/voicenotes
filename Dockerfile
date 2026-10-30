# Stage 1: Build the Go daemon.
# Pin the builder to the native build host ($BUILDPLATFORM) and cross-compile to
# the requested target arch. CGO is disabled, so Go cross-compiles trivially and
# we avoid slow QEMU emulation when producing multi-arch images.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine3.23 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

# Copy sources explicitly to keep the module-download layer cache-stable.
COPY cmd/ ./cmd/
COPY internal/ ./internal/

# Compile a statically linked binary (CGO_ENABLED=0 to avoid libc dependencies)
# for the platform buildx is currently targeting (TARGETOS/TARGETARCH are
# injected automatically per --platform entry). VERSION, COMMIT and BUILDSTAMP
# are passed by the release build; .git is not in the build context.
ARG TARGETOS TARGETARCH
ARG VERSION=dev COMMIT=unknown BUILDSTAMP=unknown
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-w -s -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.Buildstamp=${BUILDSTAMP}" \
    -o /app/bin/voicenotes ./cmd/voicenotes

# Distroless has no shell, so prepare the data directory here. It is copied
# with its ownership below, so the nonroot user can write SQLite and audio.
RUN mkdir /data && chown 65532:65532 /data

# Stage 2: Run target.
# Distroless static already ships ca-certificates (needed for the Telegram,
# speech-to-text, LLM and Memos HTTPS calls) and a nonroot (65532) user.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=builder /app/bin/voicenotes /app/voicenotes
COPY --from=builder --chown=65532:65532 /data /data

USER 65532:65532
ENV DATA_DIR=/data
VOLUME /data
EXPOSE 8080

# The worker reports not-alive until it has started, so allow a start period.
HEALTHCHECK --start-period=10s CMD ["/app/voicenotes","health"]

ENTRYPOINT ["/app/voicenotes"]
CMD ["run"]
