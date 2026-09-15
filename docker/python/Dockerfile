# polly reference image: base plus Python 3 with pip and venv from Debian.
# Build it with: polly sandbox build python (builds base first).
FROM polly/base:latest
USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 python3-pip python3-venv \
 && rm -rf /var/lib/apt/lists/*
USER 1000:1000
CMD ["sleep", "infinity"]
