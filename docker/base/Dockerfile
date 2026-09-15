# polly reference image: base.
#
# Builds polly from the repository at POLLY_REPO checked out at POLLY_REF (a
# branch, tag or commit) and installs what a workspace needs: git, bash,
# coreutils and CA certificates. The docker tool backend runs this image as
# the host user with a read-only root filesystem, a tmpfs /tmp and a tmpfs
# home, so nothing here relies on writable image state.
# Build it with: polly sandbox build base
ARG POLLY_REPO=https://github.com/alexschlessinger/polly.git
ARG POLLY_REF=main
FROM golang:1.27@sha256:0ecdc2a9f6156af6451080bfe3d8382a662fcc4e209608c6f919e643453514c1 AS build
ARG POLLY_REPO
ARG POLLY_REF
ENV CGO_ENABLED=0
RUN git init -q /src \
 && cd /src \
 && git remote add origin "$POLLY_REPO" \
 && git fetch -q --depth 1 origin "$POLLY_REF" \
 && git checkout -q FETCH_HEAD \
 && go build -o /out/polly ./cmd/polly

FROM debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818
RUN apt-get update \
 && apt-get install -y --no-install-recommends git bash coreutils ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/polly /usr/local/bin/polly
USER 1000:1000
CMD ["sleep", "infinity"]
