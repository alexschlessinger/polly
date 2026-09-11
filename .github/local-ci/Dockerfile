FROM golang:1.27@sha256:0ecdc2a9f6156af6451080bfe3d8382a662fcc4e209608c6f919e643453514c1

ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends bubblewrap python3 libicu76 \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1000 ci
ENV HOME=/home/ci GOPATH=/home/ci/go GOCACHE=/home/ci/.cache/go-build
ENV RUNNER_TOOL_CACHE=/home/ci/actions-runner/_work/_tool
WORKDIR /home/ci
COPY --chown=ci:ci go.mod go.sum /tmp/polly-deps/
COPY entrypoint.sh /usr/local/bin/polly-ci-entrypoint
USER ci
RUN cd /tmp/polly-deps && go mod download
RUN set -eu; \
    case "$TARGETARCH" in \
      arm64) runner_arch=arm64; cache_arch=arm64; runner_sha=9b1dc70626422526e3c94767cf024896beb15da5342a3f4819bf2feac13e0393 ;; \
      amd64) runner_arch=x64; cache_arch=x64; runner_sha=70920811a4f8ad4328818682bca5c6469c1c942fab52448868071d0063816613 ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    mkdir -p actions-runner "$RUNNER_TOOL_CACHE/go/1.27.0"; \
    curl -fsSL --retry 3 -o /tmp/runner.tar.gz "https://github.com/actions/runner/releases/download/v2.337.0/actions-runner-linux-${runner_arch}-2.337.0.tar.gz"; \
    echo "$runner_sha  /tmp/runner.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/runner.tar.gz -C actions-runner; rm /tmp/runner.tar.gz; \
    ln -s /usr/local/go "$RUNNER_TOOL_CACHE/go/1.27.0/$cache_arch"; \
    touch "$RUNNER_TOOL_CACHE/go/1.27.0/$cache_arch.complete"
# Seed immutable compile caches from a trusted commit. This does not run tests;
# every job runs the actual suites, and never writes its cache back to the image.
ARG CACHE_REVISION=30e4931e1dedcb8cb72306ac7222b4819e22e842
RUN export GOMAXPROCS=6 GOFLAGS=-p=6; \
    mkdir /tmp/polly-warmup && \
    curl -fsSL --retry 3 "https://github.com/alexschlessinger/polly/archive/${CACHE_REVISION}.tar.gz" \
      | tar -xz --strip-components=1 -C /tmp/polly-warmup && \
    cd /tmp/polly-warmup && \
    CGO_ENABLED=0 go build ./... && \
    CGO_ENABLED=0 go test -run '^$' ./... && \
    CGO_ENABLED=1 go test -race -run '^$' ./tools ./sessions ./cmd/polly ./llm ./subagent ./swarm ./workflow ./worktree && \
    for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do \
      CGO_ENABLED=0 GOOS=${target%/*} GOARCH=${target#*/} go build -o /tmp/polly-cross ./cmd/polly || exit; \
    done && rm -rf /tmp/polly-warmup /tmp/polly-cross
ENTRYPOINT ["/bin/bash", "/usr/local/bin/polly-ci-entrypoint"]
