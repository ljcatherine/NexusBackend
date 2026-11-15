FROM node:22-bookworm-slim@sha256:e9f565d32859f0cbfb52125eafe8be39390f24dac302dc3cdf566e8ba3ab21ee
ARG DSH_VERSION
LABEL org.nexus.dsh.version=$DSH_VERSION org.nexus.dsh.protocol="1"
WORKDIR /opt/dsh
COPY package.json package-lock.json ./
# Exact dependency tree from the reviewed lockfile; no install scripts run.
RUN npm ci --ignore-scripts --no-audit --no-fund && npm cache clean --force \
    && test "$(node_modules/.bin/dsh --version)" = "$DSH_VERSION"
COPY worker.mjs mcp.mjs /opt/nexus/
ENV HOME=/tmp NODE_ENV=production
USER 65534:65534
WORKDIR /tmp
ENTRYPOINT ["/usr/local/bin/node", "/opt/nexus/worker.mjs"]
