// Package mailparse produces the "parsed view" of a raw RFC 5322 message —
// inbound mail and the retained copy of sent outbound mail (decision 9 / Slice
// 4b-3). It derives two representations server-side: the injection-reduced text
// an agent feeds to a model by default (prefer the text/plain part, else
// HTML→text, strip quoted reply chains / forwarded headers, cap the length as a
// token-stuffing guard), and the decoded text/html part for human display.
//
// It is a CONVENIENCE, not a security control: the raw message is always
// available, and the security decision is made on structured metadata (the
// auth verdict + sender provenance, which survive as fields), never on this
// stripped text. So over-stripping is acceptable — the caller can fall back to
// raw. Best-effort throughout: a parse failure yields the best text available
// (possibly empty), never an error.
package mailparse

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// DefaultMaxBytes caps the parsed text. Generous enough for real messages,
// small enough to blunt token-stuffing. Callers may override.
const DefaultMaxBytes = 16 * 1024

// MaxHTMLBytes caps the display HTML (ParsedView.HTML). Unlike the text view —
// which is capped tight as a token-stuffing guard for model input — the HTML is
// for human display, so the bound is generous: just a backstop against a
// pathological multi-MB part bloating every detail response. raw_message stays
// the full-fidelity escape hatch.
const MaxHTMLBytes = 1 << 20 // 1 MiB

// maxMIMEDepth bounds multipart-nesting recursion. mime/multipart re-scans
// buffered data at each level, so an attacker-nested message is O(depth²) — a
// ~2MB email (under the 10MB inbound cap) could otherwise pin a request
// goroutine for minutes, and the parsed view is computed synchronously on every
// read. Real mail nests a handful of levels; past this we bail to best-effort
// (empty), consistent with the package's "over-stripping is acceptable" stance.
const maxMIMEDepth = 32

// ParsedView is the derived view of a raw RFC 5322 message: the
// injection-reduced Text an agent feeds a model by default, plus the decoded
// HTML part for human display.
type ParsedView struct {
	// Text is the injection-reduced plain text: text/plain preferred (else
	// HTML→text), quoted reply/forward chains stripped, capped at maxBytes.
	Text string
	// Truncated is true when the cap cut Text.
	Truncated bool
	// HTML is the decoded text/html part for display, or "" when the message
	// carries no HTML part. Full fidelity (NOT quote-stripped, unlike Text) and
	// capped at MaxHTMLBytes; raw_message stays the canonical copy.
	HTML string
}

// Parse extracts the derived view from a raw RFC 5322 message in a single MIME
// walk. maxBytes <= 0 uses DefaultMaxBytes (applies to Text only). Best-effort:
// a parse failure yields the best content available, never an error.
func Parse(raw []byte, maxBytes int) ParsedView {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	bestText, htmlRaw := extractText(raw)
	text := normalizeWhitespace(stripQuotedReplies(bestText))
	truncated := false
	if len(text) > maxBytes {
		text = strings.TrimSpace(text[:runeBoundary(text, maxBytes)])
		truncated = true
	}
	html := htmlRaw
	if len(html) > MaxHTMLBytes {
		html = html[:runeBoundary(html, MaxHTMLBytes)]
	}
	return ParsedView{Text: text, Truncated: truncated, HTML: html}
}

// ParsedBody extracts the injection-reduced text (see ParsedView.Text).
// truncated is true when the cap cut content. maxBytes <= 0 uses DefaultMaxBytes.
// Retained for callers that only need the text; callers wanting HTML use Parse.
func ParsedBody(raw []byte, maxBytes int) (text string, truncated bool) {
	v := Parse(raw, maxBytes)
	return v.Text, v.Truncated
}

// runeBoundary returns the largest index <= n that doesn't split a UTF-8 rune,
// so truncating at it never yields an invalid encoding.
func runeBoundary(s string, n int) int {
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return n
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// extractText walks the MIME tree and returns the best text representation —
// the first text/plain part, or (failing that) the first text/html rendered to
// text — together with the first text/html part decoded but NOT rendered to
// text (htmlRaw, for display). Falls back to the raw body if MIME parsing fails.
func extractText(raw []byte) (text, htmlRaw string) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return string(raw), ""
	}
	plain, htmlText, htmlRaw := walkParts(msg.Header.Get("Content-Type"), msg.Header.Get("Content-Transfer-Encoding"), msg.Body, 0)
	if plain != "" {
		return plain, htmlRaw
	}
	if htmlText != "" {
		return htmlText, htmlRaw
	}
	// No recognized text part — return the decoded top-level body.
	b, _ := io.ReadAll(msg.Body)
	return string(b), htmlRaw
}

// walkParts returns the first text/plain, the first text/html rendered to text,
// and the first text/html decoded as-is (htmlRaw) found anywhere in the
// (possibly nested multipart) body.
func walkParts(contentType, cte string, body io.Reader, depth int) (plain, htmlText, htmlRaw string) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		// No/!invalid Content-Type → treat as text/plain.
		return decode(body, cte), "", ""
	}
	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		if depth >= maxMIMEDepth {
			return "", "", "" // bail on pathological nesting (see maxMIMEDepth)
		}
		boundary := params["boundary"]
		if boundary == "" {
			return "", "", ""
		}
		mr := multipart.NewReader(body, boundary)
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			// Skip parts the sender flagged as attachments — they're files
			// (fetched via the attachment endpoint), not the displayable body.
			// Without this an attached .html file would surface as parsed.html.
			if disp, _, derr := mime.ParseMediaType(part.Header.Get("Content-Disposition")); derr == nil && disp == "attachment" {
				part.Close()
				continue
			}
			p, h, hr := walkParts(part.Header.Get("Content-Type"), part.Header.Get("Content-Transfer-Encoding"), part, depth+1)
			if plain == "" && p != "" {
				plain = p
			}
			if htmlText == "" && h != "" {
				htmlText = h
			}
			if htmlRaw == "" && hr != "" {
				htmlRaw = hr
			}
			part.Close()
			// Stop once both representations are in hand. Unlike the old
			// text-only walk we can't stop at text/plain alone — the text/html
			// sibling (typically later in a multipart/alternative) is the
			// display body we still need.
			if plain != "" && htmlRaw != "" {
				break
			}
		}
		return plain, htmlText, htmlRaw
	case mediaType == "text/plain":
		return decode(body, cte), "", ""
	case mediaType == "text/html":
		decoded := decode(body, cte)
		return "", htmlToText(decoded), decoded
	default:
		return "", "", ""
	}
}

// decode reads a body applying its Content-Transfer-Encoding (base64 /
// quoted-printable / identity).
func decode(body io.Reader, cte string) string {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "quoted-printable":
		b, _ := io.ReadAll(quotedprintable.NewReader(body))
		return string(b)
	case "base64":
		raw, _ := io.ReadAll(body)
		// base64 bodies are line-wrapped; strip all whitespace then decode.
		clean := strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, string(raw))
		dec, err := base64.StdEncoding.DecodeString(clean)
		if err != nil {
			return string(raw)
		}
		return string(dec)
	default:
		b, _ := io.ReadAll(body)
		return string(b)
	}
}

// HTMLToText renders an HTML body to readable plain text — the same rendition
// Parse falls back to when a message carries no text/plain part, with the
// blank-line collapsing applied so the result is usable on its own. Exported
// for outbound composition: quoting an HTML-only parent into a text/plain
// body needs exactly this rendering, and duplicating it there would let the
// two drift.
func HTMLToText(h string) string {
	return normalizeWhitespace(htmlToText(h))
}

// htmlToText renders HTML to plain text: drops script/style, inserts newlines
// at block boundaries, and decodes entities (via the tokenizer).
func htmlToText(h string) string {
	z := html.NewTokenizer(strings.NewReader(h))
	var sb strings.Builder
	skip := 0 // depth inside script/style
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return sb.String()
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			tag := string(name)
			switch tag {
			case "script", "style", "head":
				if tt == html.StartTagToken {
					skip++
				}
			case "br", "p", "div", "tr", "li", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote":
				sb.WriteByte('\n')
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style", "head":
				if skip > 0 {
					skip--
				}
			case "p", "div", "tr", "li", "blockquote":
				sb.WriteByte('\n')
			}
		case html.TextToken:
			if skip == 0 {
				sb.Write(z.Text())
			}
		}
	}
}

// quoteMarker matches the start of a quoted reply / forwarded block. Anything
// from the first match onward is dropped.
var quoteMarker = regexp.MustCompile(`(?im)^\s*(` +
	`>.*$` + // a quoted line
	`|On .+ wrote:\s*$` + // Gmail/Apple "On <date>, <name> wrote:"
	`|-{2,}\s*Original Message\s*-{2,}\s*$` + // Outlook
	`|-{2,}\s*Forwarded message\s*-{2,}\s*$` + // forwarded
	`|_{10,}\s*$` + // Outlook divider line
	`|From:\s.+\sSent:\s` + // Outlook forwarded header (From: … Sent: …)
	`)`)

// stripQuotedReplies cuts the text at the first quoted-reply / forwarded marker
// and removes any trailing ">"-quoted lines. Conservative: only well-known
// markers trigger a cut (over-stripping is acceptable; raw is always available).
func stripQuotedReplies(text string) string {
	if loc := quoteMarker.FindStringIndex(text); loc != nil {
		text = text[:loc[0]]
	}
	return strings.TrimRight(text, " \t\r\n")
}

// normalizeWhitespace collapses runs of blank lines and trims trailing spaces
// per line, so the parsed view is compact without losing paragraph structure.
func normalizeWhitespace(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t\r")
		if ln == "" {
			blank++
			if blank > 1 {
				continue // collapse 2+ blank lines into one
			}
		} else {
			blank = 0
		}
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
