FROM golang:1.27@sha256:0ecdc2a9f6156af6451080bfe3d8382a662fcc4e209608c6f919e643453514c1

ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends bubblewrap python3 libicu76 \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1000 ci
ENV HOME=/home/ci GOPATH=/home/ci/go GOCACHE=/home/ci/.cache/go-build
ENV RUNNER_TOOL_CACHE=/home/ci/actions-runner/_work/_tool
WORKDIR /home/ci
USER ci
RUN set -eu; \
    case "$TARGETARCH" in \
      arm64) arch=arm64; runner_sha=9b1dc70626422526e3c94767cf024896beb15da5342a3f4819bf2feac13e0393 ;; \
      amd64) arch=x64; runner_sha=70920811a4f8ad4328818682bca5c6469c1c942fab52448868071d0063816613 ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    mkdir -p actions-runner "$RUNNER_TOOL_CACHE/go/1.27.0"; \
    curl -fsSL --retry 3 -o /tmp/runner.tar.gz "https://github.com/actions/runner/releases/download/v2.337.0/actions-runner-linux-${arch}-2.337.0.tar.gz"; \
    echo "$runner_sha  /tmp/runner.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/runner.tar.gz -C actions-runner; rm /tmp/runner.tar.gz; \
    ln -s /usr/local/go "$RUNNER_TOOL_CACHE/go/1.27.0/$arch"; \
    touch "$RUNNER_TOOL_CACHE/go/1.27.0/$arch.complete"
# Seed immutable compile caches from a trusted commit with the current ci.sh, so
# the warmed modes are the ones jobs run. This does not run tests; every job runs
# the actual suites, and never writes its cache back to the image. The layers
# below change more often than this one and are ordered after it.
ARG CACHE_REVISION=30e4931e1dedcb8cb72306ac7222b4819e22e842
COPY --chown=ci:ci ci.sh /tmp/polly-ci.sh
RUN export GOMAXPROCS=6 GOFLAGS=-p=6; \
    mkdir /tmp/polly-warmup && \
    curl -fsSL --retry 3 "https://github.com/alexschlessinger/polly/archive/${CACHE_REVISION}.tar.gz" \
      | tar -xz --strip-components=1 -C /tmp/polly-warmup && \
    mv /tmp/polly-ci.sh /tmp/polly-warmup/.github/ci.sh && \
    /bin/bash /tmp/polly-warmup/.github/ci.sh warm && \
    rm -rf /tmp/polly-warmup
COPY --chown=ci:ci go.mod go.sum /tmp/polly-deps/
RUN cd /tmp/polly-deps && go mod download
COPY entrypoint.sh /usr/local/bin/polly-ci-entrypoint
ENTRYPOINT ["/bin/bash", "/usr/local/bin/polly-ci-entrypoint"]
