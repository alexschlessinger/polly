# polly reference image: base plus Node.js and npm from Debian.
# Build it with: polly sandbox build node (builds base first).
FROM polly/base:latest
USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends nodejs npm \
 && rm -rf /var/lib/apt/lists/*
USER 1000:1000
CMD ["sleep", "infinity"]
