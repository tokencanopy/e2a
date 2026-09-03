# syntax=docker/dockerfile:1
#
# Multi-stage build for @e2a/mcp-server's HTTP bin. The image runs the
# bin published as `node mcp/dist/bin/http.js`; the stdio bin is unused
# here but ships in the same artifact so we don't fork the build.

FROM node:22-alpine AS deps
WORKDIR /app
# Copy only manifests + the lockfile so this layer caches across code changes.
#
# EVERY workspace in the root `workspaces` array must be copied, design-system
# included. npm resolves the whole workspace graph, so a declared-but-absent
# member leaves a dangling edge; `npm ci` rejects that outright rather than
# guessing, which is the behaviour we want from a release build.
COPY package.json package-lock.json ./
COPY sdks/typescript/package.json sdks/typescript/
COPY mcp/package.json mcp/
COPY cli/package.json cli/
COPY design-system/package.json design-system/
# `npm ci` installs exactly the committed lockfile. The previous
# `npm install --package-lock=false` threw that lockfile away and re-resolved
# from the live registry on every build, which made the image a function of
# what npm happened to serve that minute rather than of the commit.
#
# That is not theoretical. On 2026-09-03 the MCP image built successfully at
# 05:14:03; `@e2a/sdk@5.9.0` was published to npm at 05:15:45; and every build
# afterwards died in arborist with `Cannot read properties of null (reading
# 'edgesOut')` — same commit, same Dockerfile, different registry. Once the
# workspace's own version also exists upstream, live resolution has two
# candidates for one edge and picks neither. The lockfile has never been
# ambiguous about it: `node_modules/@e2a/sdk` links to `sdks/typescript`.
RUN npm ci --include=dev --no-audit --no-fund

FROM node:22-alpine AS build
WORKDIR /app
# npm workspaces hoists all deps to the root node_modules.
COPY --from=deps /app/node_modules ./node_modules
COPY --from=deps /app/package.json /app/package-lock.json ./
COPY --from=deps /app/sdks/typescript/package.json sdks/typescript/
COPY --from=deps /app/mcp/package.json mcp/
COPY --from=deps /app/cli/package.json cli/
COPY --from=deps /app/design-system/package.json design-system/
COPY sdks/typescript ./sdks/typescript
COPY mcp ./mcp
RUN npm run build --workspace @e2a/sdk
RUN npm run build --workspace @e2a/mcp-server

FROM node:22-alpine AS runtime
WORKDIR /app
ARG MCP_SERVER_VERSION
ENV NODE_ENV=production
ENV MCP_SERVER_VERSION=${MCP_SERVER_VERSION}
# Fresh install with production-only deps so the runtime image is lean — from
# the same lockfile, so the runtime tree is a strict subset of the one the build
# stage compiled against rather than a second independent resolution.
COPY --from=build /app/package.json /app/package-lock.json ./
COPY --from=build /app/sdks/typescript/package.json sdks/typescript/
COPY --from=build /app/mcp/package.json mcp/
COPY --from=build /app/cli/package.json cli/
COPY --from=build /app/design-system/package.json design-system/
RUN npm ci --omit=dev --no-audit --no-fund \
  && npm cache clean --force
# Built JS only — no TypeScript source ships in the image.
COPY --from=build /app/sdks/typescript/dist ./sdks/typescript/dist
COPY --from=build /app/mcp/dist ./mcp/dist
COPY VERSION ./VERSION
EXPOSE 3000
USER node
# Liveness only: /healthz never touches the backend, so a backend outage
# can't restart a healthy MCP process. Probe with node's built-in fetch
# (always present; the image has no curl, only busybox wget). Readiness
# (/readyz) is an orchestrator-level concern and deliberately not baked in.
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=5 \
    CMD node -e "fetch('http://127.0.0.1:3000/healthz').then(r => process.exit(r.ok ? 0 : 1)).catch(() => process.exit(1))"
CMD ["node", "mcp/dist/bin/http.js"]
