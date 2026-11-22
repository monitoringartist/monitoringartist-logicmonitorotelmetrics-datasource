# syntax=docker/dockerfile:1

# Dockerfile for Grafana Assistant managed sandbox
# Preinstalled development tools for AI agent coding of Grafana plugins (backend and frontend)

ARG ALPINE_VERSION=3.24
ARG GO_VERSION=1.27.4
ARG NODE_VERSION=24.22.2
ARG UV_VERSION=0.13.9

FROM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS go-toolchain

ARG MAGE_VERSION=v1.17.2
ARG GOPLS_VERSION=v0.24.0
ARG GOTOOLS_VERSION=v0.53.0
ARG DELVE_VERSION=v1.27.2

ENV CGO_ENABLED=0 \
    GOBIN=/out

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go install github.com/magefile/mage@${MAGE_VERSION} && \
    go install golang.org/x/tools/gopls@${GOPLS_VERSION} && \
    go install golang.org/x/tools/cmd/goimports@${GOTOOLS_VERSION} && \
    go install github.com/go-delve/delve/cmd/dlv@${DELVE_VERSION}

FROM node:${NODE_VERSION}-alpine${ALPINE_VERSION} AS node-toolchain

FROM ghcr.io/astral-sh/uv:${UV_VERSION} AS uv-toolchain

FROM alpine:${ALPINE_VERSION}

ARG TARGETARCH
ARG GOLANGCI_LINT_VERSION=2.15.0
ARG GH_VERSION=2.105.0
ARG NPM_VERSION=11.19.0
ARG CREATE_PLUGIN_VERSION=9.0.4
ARG PLUGIN_VALIDATOR_VERSION=0.53.6

ENV GOPATH=/go \
    PATH="/go/bin:/usr/local/go/bin:${PATH}" \
    UV_LINK_MODE=copy \
    UV_TOOL_BIN_DIR=/usr/local/bin \
    COREPACK_ENABLE_DOWNLOAD_PROMPT=0

RUN apk add --no-cache \
    bash \
    build-base \
    ca-certificates \
    curl \
    docker-cli \
    docker-cli-buildx \
    docker-cli-compose \
    fd \
    git \
    jq \
    libstdc++ \
    make \
    openssh-client \
    python3 \
    ripgrep \
    tar \
    unzip \
    wget \
    zip

SHELL ["/bin/bash", "-eo", "pipefail", "-c"]

COPY --from=go-toolchain /usr/local/go /usr/local/go
COPY --from=go-toolchain /out/ /usr/local/bin/
COPY --from=node-toolchain /usr/local/bin/node /usr/local/bin/node
COPY --from=node-toolchain /usr/local/lib/node_modules /usr/local/lib/node_modules
COPY --from=uv-toolchain /uv /uvx /bin/

RUN ARCH="${TARGETARCH:-$(uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')}" && \
    curl -fsSL "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-${ARCH}.tar.gz" \
      | tar -xz -C /usr/local/bin --strip-components=1 "golangci-lint-${GOLANGCI_LINT_VERSION}-linux-${ARCH}/golangci-lint" && \
    curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${ARCH}.tar.gz" \
      | tar -xz -C /usr/local/bin --strip-components=2 "gh_${GH_VERSION}_linux_${ARCH}/bin/gh"

RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm && \
    ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx && \
    ln -s ../lib/node_modules/corepack/dist/corepack.js /usr/local/bin/corepack && \
    corepack enable --install-directory /usr/local/bin yarn pnpm && \
    npm install -g --no-fund --no-audit \
      "npm@${NPM_VERSION}" \
      "@grafana/create-plugin@${CREATE_PLUGIN_VERSION}" \
      "@grafana/plugin-validator@${PLUGIN_VALIDATOR_VERSION}" && \
    npm cache clean --force && \
    rm -rf /root/.npm /tmp/*

RUN mkdir -p "${GOPATH}/bin" && \
    git --version && \
    gh --version && \
    curl --version | head -n 1 && \
    wget --version 2>&1 | head -n 1 && \
    jq --version && \
    rg --version | head -n 1 && \
    fd --version && \
    make --version | head -n 1 && \
    gcc --version | head -n 1 && \
    zip -v | head -n 2 | tail -n 1 && \
    unzip -v | head -n 1 && \
    docker --version && \
    docker compose version && \
    python3 --version && \
    uv --version && \
    uvx --version && \
    go version && \
    mage -version && \
    golangci-lint version && \
    gopls version && \
    command -v goimports && \
    dlv version && \
    node --version && \
    npm --version && \
    npx --version && \
    corepack --version && \
    command -v yarn pnpm && \
    command -v create-plugin && \
    command -v plugin-validator

WORKDIR /workspace
CMD ["/bin/bash"]
