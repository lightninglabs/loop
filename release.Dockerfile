FROM golang:1.24.10

RUN apt-get update && apt-get install -y --no-install-recommends \
    git ca-certificates zip gpg && rm -rf /var/lib/apt/lists/*

# Add GPG key of Alex Bosworth to verify release tag signature.
RUN gpg --keyserver keys.openpgp.org \
    --recv-keys DE23E73BFA8A0AD5587D2FCDE80D2F3F311FD87E

# Mark the repo directory safe for git. User ID may be different inside
# Docker and Git might refuse to work without this setting.
RUN git config --global --add safe.directory /repo
RUN git config --global --add safe.directory /repo/.git

# Set GO build time environment variables.
ENV GOCACHE=/tmp/build/.cache \
    GOMODCACHE=/tmp/build/.modcache

# Create directories to which host's Go caches are mounted.
RUN mkdir -p /tmp/build/.cache /tmp/build/.modcache \
    && chmod -R 777 /tmp/build/

WORKDIR /repo
