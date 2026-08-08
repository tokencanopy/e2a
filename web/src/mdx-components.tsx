import type { MDXComponents } from "mdx/types";

// Map MDX elements to styled HTML for the blog.
// Keep this list minimal — post-specific overrides can be passed in per-page.
//
// Colors MUST come from Loft tokens (`var(--…)`), never literal hex. These are
// inline styles, so a `@media (prefers-color-scheme)` rule cannot reach them;
// the tokens are what makes the article body follow the `.dark` class that
// ThemeProvider puts on <html>. Hardcoded light-theme hex here is what made
// blog posts render near-invisible (h1 at 1.09:1) for dark-mode visitors.
export function useMDXComponents(components: MDXComponents): MDXComponents {
  return {
    h1: ({ children }) => (
      <h1
        style={{
          fontFamily: "var(--f-editorial)",
          fontSize: "clamp(32px, 3.5vw, 44px)",
          lineHeight: 1.15,
          fontWeight: 400,
          color: "var(--fg)",
          margin: "0 0 18px",
          letterSpacing: "-0.01em",
        }}
      >
        {children}
      </h1>
    ),
    h2: ({ children }) => (
      <h2
        style={{
          fontFamily: "var(--f-editorial)",
          fontSize: 26,
          fontWeight: 400,
          color: "var(--fg)",
          margin: "36px 0 12px",
          letterSpacing: "-0.005em",
        }}
      >
        {children}
      </h2>
    ),
    h3: ({ children }) => (
      <h3 style={{ fontSize: 18, fontWeight: 600, color: "var(--fg)", margin: "28px 0 10px" }}>
        {children}
      </h3>
    ),
    p: ({ children }) => (
      <p style={{ fontSize: 16, lineHeight: 1.7, color: "var(--fg)", margin: "0 0 16px" }}>
        {children}
      </p>
    ),
    a: ({ href, children }) => (
      <a
        href={href}
        style={{
          color: "var(--accent-strong)",
          textDecoration: "underline",
          textUnderlineOffset: "3px",
        }}
      >
        {children}
      </a>
    ),
    ul: ({ children }) => (
      <ul style={{ fontSize: 16, lineHeight: 1.7, color: "var(--fg)", margin: "0 0 16px", paddingLeft: 22 }}>
        {children}
      </ul>
    ),
    ol: ({ children }) => (
      <ol style={{ fontSize: 16, lineHeight: 1.7, color: "var(--fg)", margin: "0 0 16px", paddingLeft: 22 }}>
        {children}
      </ol>
    ),
    li: ({ children }) => <li style={{ margin: "0 0 6px" }}>{children}</li>,
    code: ({ children, ...props }) => {
      // Inline code when no className (block code gets className="language-*")
      if (!("className" in (props as Record<string, unknown>))) {
        return (
          <code
            style={{
              fontFamily: "var(--f-mono)",
              fontSize: "0.92em",
              background: "var(--bg-elev)",
              padding: "1px 6px",
              borderRadius: 4,
              border: "0.5px solid var(--border)",
              color: "var(--fg)",
            }}
          >
            {children}
          </code>
        );
      }
      return <code {...props}>{children}</code>;
    },
    // Code blocks stay on the ink surface in both themes — dark-on-cream in
    // light mode, sunken below --bg in dark mode. --ink-border keeps the edge
    // from haloing on cream while still separating the block in dark mode.
    pre: ({ children }) => (
      <pre
        style={{
          background: "var(--ink)",
          color: "var(--ink-fg)",
          border: "1px solid var(--ink-border)",
          padding: "20px 24px",
          borderRadius: 10,
          fontFamily: "var(--f-mono)",
          fontSize: 13,
          lineHeight: 1.7,
          overflowX: "auto",
          margin: "20px 0",
        }}
      >
        {children}
      </pre>
    ),
    blockquote: ({ children }) => (
      <blockquote
        style={{
          borderLeft: "3px solid var(--border-strong)",
          paddingLeft: 18,
          margin: "20px 0",
          color: "var(--fg-muted)",
          fontStyle: "italic",
        }}
      >
        {children}
      </blockquote>
    ),
    hr: () => (
      <hr style={{ border: "none", borderTop: "0.5px solid var(--border)", margin: "36px 0" }} />
    ),
    ...components,
  };
}
