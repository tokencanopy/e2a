# syntax=docker/dockerfile:1.6
#
# Two-stage build: Next.js produces a static export, Caddy serves it
# with a parameterizable reverse-proxy to the e2a API backend.
#
# Build context is the repo ROOT (web depends on the sibling @e2a/ui workspace
# via file:../design-system, which must be inside the context):
#   docker build -t e2a-web -f web/Dockerfile .
#
# Customizing branding/SEO for a non-default deployment:
#   docker build \
#     --build-arg NEXT_PUBLIC_SITE_URL=https://e2a.example.com \
#     --build-arg NEXT_PUBLIC_AGENTS_DOMAIN=agents.example.com \
#     -t e2a-web -f web/Dockerfile web
#
# Next.js inlines NEXT_PUBLIC_* values at build time (a static-export
# constraint), so these cannot be overridden at container start. To
# rebrand a published image, rebuild from source with --build-arg.

# ── Builder ─────────────────────────────────────────────────────────
# Pin to the host's native arch — Next.js' static export is HTML/CSS/JS,
# so the build itself is arch-neutral. Native npm deps that the builder
# pulls (@next/swc-*, esbuild) match the host platform automatically,
# which is what we want. The runtime stage stays per-target so each arch
# gets its own caddy binary.
FROM --platform=$BUILDPLATFORM node:22-alpine AS builder
WORKDIR /app

# @e2a/ui is a sibling workspace consumed via file:../design-system. Copy it
# (with its committed dist/) so web's file: symlink resolves during install and
# `next build`. No build step needed here — dist/ is prebuilt and committed.
COPY design-system ./design-system

# The web prebuild mirrors canonical agent docs into web/public. Keep the
# script and only its canonical inputs in the image build context.
COPY scripts/sync-agent-docs.mjs ./scripts/
COPY plugins/e2a/docs ./plugins/e2a/docs/

# Install web deps first so the layer caches when only source changes.
COPY web/package.json web/package-lock.json ./web/
WORKDIR /app/web
RUN npm ci --no-audit --prefer-offline

# Empty defaults make the published OSS image generic. The hosted
# deployment overrides these at build time. See web/.env.example for
# the full list and what each one controls.
ARG NEXT_PUBLIC_SITE_URL=
ARG NEXT_PUBLIC_SITE_NAME=
ARG NEXT_PUBLIC_AGENTS_DOMAIN=
ARG NEXT_PUBLIC_FEEDBACK_EMAIL=
ARG NEXT_PUBLIC_GOOGLE_SITE_VERIFICATION=
# Empty means this deployment has no pricing page, so the sitemap omits it.
# The hosted deployment serves /pricing from the ops repo through Caddy and
# sets this to /pricing; staging and self-hosts leave it unset.
ARG NEXT_PUBLIC_PRICING_PATH=
# Empty disables the dashboard's Upgrade / Manage Billing affordances —
# self-host deployments without a paid tier leave this unset. The
# hosted deployment sets it to the dashboard's own origin (e.g.
# https://e2a.dev) so checkout/portal POSTs land on the Caddy-fronted
# sidecar at /api/billing/*.
ARG NEXT_PUBLIC_BILLING_API=
# Empty keeps the dashboard's sign-in links on the legacy Google door
# (/api/auth/login), which every deployment has. The hosted deployment sets
# this to /api/auth/oidc/login so the generic OIDC door is the default. The
# upstream hosted site identity brands that door as TokenCanopy; self-hosted
# builds remain provider-neutral. Only set this when the server runs with
# E2A_OIDC_ENABLED=true — the OIDC route doesn't exist otherwise.
ARG NEXT_PUBLIC_E2A_SIGN_IN_URL=
# Empty keeps the site free of web analytics. The hosted deployment sets
# this to the e2a.dev Umami website record id so the first-party tracker
# loads on public marketing routes only (the dashboard excludes it).
ARG NEXT_PUBLIC_UMAMI_WEBSITE_ID=

ENV NEXT_PUBLIC_SITE_URL=$NEXT_PUBLIC_SITE_URL \
    NEXT_PUBLIC_SITE_NAME=$NEXT_PUBLIC_SITE_NAME \
    NEXT_PUBLIC_AGENTS_DOMAIN=$NEXT_PUBLIC_AGENTS_DOMAIN \
    NEXT_PUBLIC_FEEDBACK_EMAIL=$NEXT_PUBLIC_FEEDBACK_EMAIL \
    NEXT_PUBLIC_GOOGLE_SITE_VERIFICATION=$NEXT_PUBLIC_GOOGLE_SITE_VERIFICATION \
    NEXT_PUBLIC_PRICING_PATH=$NEXT_PUBLIC_PRICING_PATH \
    NEXT_PUBLIC_BILLING_API=$NEXT_PUBLIC_BILLING_API \
    NEXT_PUBLIC_E2A_SIGN_IN_URL=$NEXT_PUBLIC_E2A_SIGN_IN_URL \
    NEXT_PUBLIC_UMAMI_WEBSITE_ID=$NEXT_PUBLIC_UMAMI_WEBSITE_ID

COPY web ./
RUN npm run build

# ── Runtime ─────────────────────────────────────────────────────────
# Caddy 2 serves the static export and reverse-proxies /api/* to the
# e2a Go backend. The Caddyfile uses {$VAR:default} substitution so
# operators can rewire the proxy target without editing the file:
#
#   docker run \
#     -e E2A_BACKEND=http://e2a-server:8080 \
#     -e WEB_PORT=8081 \
#     -p 8081:8081 ghcr.io/tokencanopy/e2a-web
FROM caddy:2-alpine
# wget is provided by alpine's busybox applet, but caddy:2-alpine doesn't
# guarantee it stays — Caddy's official Dockerfile doesn't `apk add` it.
# Install explicitly so the HEALTHCHECK below can't silently break if a
# future base image trims busybox.
RUN apk add --no-cache wget
COPY --from=builder /app/web/out /srv
COPY web/Caddyfile /etc/caddy/Caddyfile

# Default port; override with WEB_PORT env. Healthcheck reads the same
# var so it works regardless of the chosen port.
ENV WEB_PORT=80
EXPOSE 80
# --no-check-certificate + --max-redirect handle the auto-HTTPS case:
# operators who terminate TLS in this Caddy (e.g. a Let's Encrypt
# site block instead of `auto_https off`) will see the plain-HTTP
# request 308-redirect to https://127.0.0.1, where the cert is for
# their public hostname, not localhost. Skipping cert validation on
# a self-loopback healthcheck is safe — we're asking "is *this*
# container's Caddy responding?", not "is the cert valid for some
# external client?". Operators with `auto_https off` (the default
# Caddyfile shipped here) get plain HTTP, no redirect, same path
# as before.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-check-certificate -q -O - --max-redirect=5 \
        "http://127.0.0.1:${WEB_PORT}/" >/dev/null || exit 1
