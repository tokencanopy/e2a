# design-sync notes — @e2a/ui (Loft)

Durable, human-readable notes for future syncs. Committed.

## Setup facts

- **Monorepo layout.** This package lives inside the **e2a** git repo as an npm
  workspace (`@e2a/ui`, declared in the repo-root `package.json` workspaces).
  The git root is therefore `…/e2a`, NOT this folder. The converter resolves its
  paths from the **working directory**, so always run design-sync commands from
  **this `design-system/` directory**, not the repo root.
- **Re-sync command (monorepo).** React is hoisted to the repo-root
  `node_modules`, so point `--node-modules` there. From `design-system/`:
  ```sh
  npm run build                               # refresh dist/ first
  # rebuild the reference explicitly under THIS package (do NOT use the
  # git-root path the storybook §2.2 snippet suggests — that lands at e2a/.design-sync):
  npx storybook build -c .storybook -o .design-sync/sb-reference
  node .ds-sync/resync.mjs --config .design-sync/config.json \
    --node-modules ../node_modules --entry ./dist/index.js \
    --out ./ds-bundle --remote .design-sync/.cache/remote-sync.json
  ```
  (Re-stage `.ds-sync/` first per storybook §2.4 if it's missing — it's gitignored.)
- **Shape:** storybook. Reference built into `.design-sync/sb-reference` from `.storybook/`.
- **Own-source-repo build (generic case — does not apply in this monorepo):**
  outside a monorepo layout, where `@e2a/ui` isn't hoisted into a shared
  `node_modules`, the converter would instead run with
  `--entry ./dist/index.js` and `--node-modules ./node_modules` (this
  package's own). **In this repo, use the monorepo command above
  (`--node-modules ../node_modules`) instead** — React is hoisted to the
  repo-root `node_modules`, not `design-system/node_modules`. Always
  `npm run build` before the converter so `dist/` is fresh.
- **CSS is flattened at build time.** `scripts/flatten-css.mjs` (wired into tsup
  `onSuccess`) inlines `src/tokens.css` into `dist/styles.css` so the shipped
  stylesheet is self-contained (no relative `@import` the converter would drop).
  `[GENERAL]` If `dist/styles.css` ever `@import`s a sibling again, the converter
  ships a bundle whose `var(--*)` are all undefined → everything renders unstyled.

## Consuming from Next.js App Router

- **The bundle ships `"use client"`** (tsup `banner`). The library mixes
  interactive components (`InkConsole`, `Collapsible`) that use React hooks with
  pure ones; esbuild strips per-file directives when bundling, so the whole entry
  is marked a client module. Without this, importing `@e2a/ui` into a Server
  Component fails (`"You're importing a module that depends on useEffect…"`).
  Tradeoff: every component is a client component (no RSC for the pure ones). A
  future refinement is per-component directive preservation (split build) so
  `Button`/`Chip`/`Card`/etc. can stay server components.
- The `"use client"` banner is a no-op for the design-sync esbuild bundle and for
  Storybook (both ignore the directive). It does change `dist/index.js` bytes, so
  the next design-sync re-sync will re-upload the bundle once — harmless.

## Accepted deviations (the oracle can't see these)

- **`[FONT_MISSING]` — accepted, system substitutes.** The CSS references
  `"Inter"` (`--f-ui`) and `"JetBrains Mono"` (`--f-mono`), but this package does
  not vendor the woff2 files (the app loads them via `next/font`). Both panels of
  the compare oracle fall back to the same system fonts, so grades pass while
  claude.ai/design users see system sans/mono instead of Inter/JetBrains Mono.
  This is acceptable for now — the tokens declare close system fallbacks
  (`-apple-system …`, `ui-monospace …`). **To make it faithful:** add the woff2s
  (e.g. the `@fontsource/inter` and `@fontsource/jetbrains-mono` packages) and
  wire `cfg.extraFonts` with matching `@font-face` blocks, then inject the same
  `@font-face` into `.design-sync/sb-reference/iframe.html` so the oracle verifies
  with the real fonts on both sides.

## Re-sync risks (watch-list for the next run)

- **Fonts:** still substituted (see above). If `cfg.extraFonts` was added since,
  confirm the woff2 paths still resolve.
- **ThemeToggle is a controlled fork.** The app's original consumed a
  `ThemeProvider` context; this package's version takes `value`/`onChange` props.
  Its stories drive it from local `useState`. If the upstream component's API
  changes, this copy won't track it automatically.
- **CSS-driven atoms.** The source components in `web/src/app/components/loft/`
  mix Tailwind utility classes with CSS-variable inline styles. The copies here
  were adapted to be fully CSS-class-driven (`loft-*` classes in `styles.css`).
  They are visual ports, not byte-identical re-exports — re-verify against the
  storybook render after any upstream restyle.
- **11 components synced**, from three sources: `loft/` (`Button`, `Chip`,
  `Dot`, `Eyebrow`, `ThemeToggle`, `InkConsole`); brand SVGs in `web/public`
  (`Logo` — one themeable component replacing the 4 static svgs); and other
  app components ported CSS-driven (`Field` ← `Field.tsx`, `Avatar` ←
  `CounterpartyAvatar`, `Collapsible` ← `messages/Collapsible.tsx`); plus
  net-new `Card`. Still NOT synced: `PageShell`/`Topbar` (responsive Tailwind
  layout — want Tailwind in the package first) and `Sidebar` (Next.js routing +
  auth context).
- **`Logo` and `Avatar` are token-faithful, not file-faithful.** `Logo` is one
  React component drawn from Loft tokens (the `web/public/*.svg` files are flat
  recolorings of the same marks); `Avatar` needs the `--av-1..8` palette, which
  was added to `tokens.css` (it was missing from the first extraction).
