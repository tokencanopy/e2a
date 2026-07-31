package outbound

import (
	"strings"
)

// EXPERIMENTAL: reply quote-history composition. Generalizes the forward
// quote block (forward.go) to the mail-client reply shape: the caller's
// reply body on top, an "On <date>, <from> wrote:" attribution line, then
// the parent body as a quote (">"-prefixed text / blockquote HTML). Reuses
// ForwardContext for header + body extraction so both composition paths
// stay lexically consistent.

// BuildReplyQuoteAttribution renders the attribution line above the quoted
// parent. Degrades gracefully when the parent's headers failed to parse.
func BuildReplyQuoteAttribution(ctx ForwardContext) string {
	from := ctx.From
	if from == "" {
		from = "the sender"
	}
	if ctx.Date != "" {
		return "On " + ctx.Date + ", " + from + " wrote:"
	}
	return from + " wrote:"
}

// BuildReplyQuoteBody composes the text/plain body of a quoted reply: the
// caller's reply text, a blank line, the attribution line, then the parent
// text with every line ">"-prefixed. A parent that already carries ">"
// quoting nests naturally (">>"), matching mail-client behavior. An empty
// parent text drops the quote block entirely — the reply goes out exactly
// as the caller wrote it.
func BuildReplyQuoteBody(replyText string, ctx ForwardContext) string {
	if ctx.Text == "" {
		return replyText
	}
	var buf strings.Builder
	buf.WriteString(strings.TrimRight(replyText, "\r\n"))
	buf.WriteString("\r\n\r\n")
	buf.WriteString(BuildReplyQuoteAttribution(ctx))
	buf.WriteString("\r\n")
	text := strings.ReplaceAll(ctx.Text, "\r\n", "\n")
	text = strings.TrimRight(text, "\n")
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, ">") {
			// Existing quote level: extend without inserting a space so
			// depth markers stay compact (">>"), the way clients emit them.
			buf.WriteString(">")
		} else {
			buf.WriteString("> ")
		}
		buf.WriteString(line)
		buf.WriteString("\r\n")
	}
	return buf.String()
}

// BuildReplyQuoteHTMLBody composes the text/html body of a quoted reply.
// The caller's HTML is emitted as-is (caller-controlled markup, same
// contract as forward); the parent HTML is wrapped in a Gmail-style
// blockquote under the attribution line. Falls back to the escaped parent
// text in <pre> when the parent has no HTML part; drops the quote block
// when the parent has neither.
func BuildReplyQuoteHTMLBody(replyHTML string, ctx ForwardContext) string {
	if ctx.HTML == "" && ctx.Text == "" {
		return replyHTML
	}
	var buf strings.Builder
	buf.WriteString(strings.TrimSpace(replyHTML))
	buf.WriteString("\r\n<br>\r\n")
	buf.WriteString(`<div class="e2a_quote">`)
	buf.WriteString("\r\n")
	buf.WriteString(htmlEscape(BuildReplyQuoteAttribution(ctx)))
	buf.WriteString("<br>\r\n")
	buf.WriteString(`<blockquote style="margin:0 0 0 0.8ex;border-left:1px solid #ccc;padding-left:1ex">`)
	buf.WriteString("\r\n")
	if ctx.HTML != "" {
		buf.WriteString(ctx.HTML)
	} else {
		buf.WriteString("<pre>")
		buf.WriteString(htmlEscape(ctx.Text))
		buf.WriteString("</pre>")
	}
	buf.WriteString("\r\n</blockquote>\r\n</div>")
	return buf.String()
}
