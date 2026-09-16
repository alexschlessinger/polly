# polly reference image: base plus the Go toolchain.
# Build it with: polly sandbox build go (builds base first).
FROM golang:1.27@sha256:0ecdc2a9f6156af6451080bfe3d8382a662fcc4e209608c6f919e643453514c1 AS toolchain

FROM polly/base:latest
COPY --from=toolchain /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH
USER 1000:1000
CMD ["sleep", "infinity"]
