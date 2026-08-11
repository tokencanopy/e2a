package outbound

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"

	"github.com/tokencanopy/e2a/internal/mailparse"
)

// ForwardContext captures the header + body fields from an inbound message
// that a forward should quote. Headers are kept as raw strings (no
// re-parsing) so the quoted block renders the same lexical text the
// original sender chose, including display names. Text/HTML are
// best-effort decoded from the raw MIME — empty strings on parse
// failure so the forward still ships with the header block.
type ForwardContext struct {
	From    string
	Date    string
	Subject string
	To      string
	Cc      string
	Text    string
	HTML    string
}

// ExtractForwardContext parses an RFC 5322 raw message and pulls out the
// fields needed to compose a forward quote. Parse failures degrade
// gracefully — the returned context's body fields stay empty so the
// caller still gets a usable header block to prepend.
func ExtractForwardContext(rawMessage []byte) ForwardContext {
	ctx := ForwardContext{}
	if len(rawMessage) == 0 {
		return ctx
	}

	msg, err := mail.ReadMessage(bytes.NewReader(rawMessage))
	if err != nil {
		return ctx
	}

	ctx.From = strings.TrimSpace(msg.Header.Get("From"))
	ctx.Date = strings.TrimSpace(msg.Header.Get("Date"))
	ctx.Subject = strings.TrimSpace(msg.Header.Get("Subject"))
	ctx.To = strings.TrimSpace(msg.Header.Get("To"))
	ctx.Cc = strings.TrimSpace(msg.Header.Get("Cc"))

	contentType := msg.Header.Get("Content-Type")
	encoding := msg.Header.Get("Content-Transfer-Encoding")
	ctx.Text, ctx.HTML = extractBodyParts(msg.Body, contentType, encoding, 0)
	return ctx
}

// maxExtractMIMEDepth bounds the multipart recursion below. Legitimate mail
// nests two or three levels (mixed > alternative > related); a crafted
// inbound under the 10 MiB cap could otherwise nest tens of thousands of
// parts and pin the request goroutine. Mirrors mailparse.maxMIMEDepth.
const maxExtractMIMEDepth = 32

// extractBodyParts walks a message body looking for the text/plain and
// text/html parts. Recurses into multipart/alternative and
// multipart/mixed. The body io.Reader is consumed in a single pass — for
// non-multipart bodies the entire reader is treated as a single part.
func extractBodyParts(body io.Reader, contentType, encoding string, depth int) (textOut, htmlOut string) {
	if depth > maxExtractMIMEDepth {
		return "", ""
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		// No Content-Type or malformed — fall through and treat as
		// text/plain. Cheaper than refusing the forward.
		mediaType = "text/plain"
		params = nil
	}

	if !strings.HasPrefix(mediaType, "multipart/") {
		raw, err := io.ReadAll(body)
		if err != nil {
			return "", ""
		}
		decoded := decodeTransferEncoding(raw, encoding)
		switch mediaType {
		case "text/html":
			return "", string(decoded)
		default:
			return string(decoded), ""
		}
	}

	boundary := params["boundary"]
	if boundary == "" {
		return "", ""
	}

	mr := multipart.NewReader(body, boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		partCT := part.Header.Get("Content-Type")
		partEnc := part.Header.Get("Content-Transfer-Encoding")
		partType, _, _ := mime.ParseMediaType(partCT)

		if strings.HasPrefix(partType, "multipart/") {
			nestedText, nestedHTML := extractBodyParts(part, partCT, partEnc, depth+1)
			if textOut == "" {
				textOut = nestedText
			}
			if htmlOut == "" {
				htmlOut = nestedHTML
			}
			_ = part.Close()
			continue
		}

		raw, err := io.ReadAll(part)
		_ = part.Close()
		if err != nil {
			continue
		}
		decoded := decodeTransferEncoding(raw, partEnc)
		switch partType {
		case "text/plain":
			if textOut == "" {
				textOut = string(decoded)
			}
		case "text/html":
			if htmlOut == "" {
				htmlOut = string(decoded)
			}
		}
		if textOut != "" && htmlOut != "" {
			break
		}
	}
	return textOut, htmlOut
}

// decodeTransferEncoding decodes the Content-Transfer-Encoding wrapping
// of a body part. Unknown encodings pass through unchanged — better to
// surface raw bytes than drop the part entirely.
func decodeTransferEncoding(data []byte, encoding string) []byte {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(data)))
		if err != nil {
			return data
		}
		return decoded
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(data)))
		if err != nil {
			return data
		}
		return decoded
	default:
		return data
	}
}

// ForwardAttachments extracts the stored attachment parts of the message being
// forwarded and converts them to the outbound Attachment shape (base64-encoded
// data) so a forward carries the original files by default, the way mail
// clients do — the server already holds the bytes, so no client round-trip is
// needed. Order matches the source message's authoritative attachment index.
// Returns nil when the source has no attachments.
func ForwardAttachments(rawMessage []byte) []Attachment {
	parts := mailparse.Attachments(rawMessage)
	if len(parts) == 0 {
		return nil
	}
	out := make([]Attachment, 0, len(parts))
	for _, p := range parts {
		out = append(out, Attachment{
			Filename:    p.Filename,
			ContentType: p.ContentType,
			Data:        base64.StdEncoding.EncodeToString(p.Data),
		})
	}
	return out
}

// BuildForwardSubject prefixes "Fwd: " unless the subject already starts
// with Fwd:, Fw:, or Re:. The dedup avoids stacking on chains. Empty
// inputs produce "Fwd: (no subject)" so the recipient still sees the
// message is a forward.
func BuildForwardSubject(orig string) string {
	trimmed := strings.TrimSpace(orig)
	if trimmed == "" {
		return "Fwd: (no subject)"
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "fwd:") || strings.HasPrefix(lower, "fw:") {
		return trimmed
	}
	return "Fwd: " + trimmed
}

// BuildForwardBody composes the text/plain body of a forward: the
// caller's optional comment, a Gmail-style divider, the original headers
// as a quote block, then the original text body if extraction
// succeeded.
func BuildForwardBody(comment string, ctx ForwardContext) string {
	var buf strings.Builder
	if c := strings.TrimRight(comment, "\r\n"); c != "" {
		buf.WriteString(c)
		buf.WriteString("\r\n\r\n")
	}
	buf.WriteString("---------- Forwarded message ---------\r\n")
	writeHeaderLine(&buf, "From", ctx.From)
	writeHeaderLine(&buf, "Date", ctx.Date)
	writeHeaderLine(&buf, "Subject", ctx.Subject)
	writeHeaderLine(&buf, "To", ctx.To)
	writeHeaderLine(&buf, "Cc", ctx.Cc)
	buf.WriteString("\r\n")
	if ctx.Text != "" {
		// Normalize line endings before re-emitting: a naive
		// ReplaceAll("\n","\r\n") turns existing "\r\n" into "\r\r\n",
		// which mail clients render as a literal CR. Real-world bodies
		// almost always arrive CRLF-terminated, so the lazy form would
		// fire on virtually every multi-line forward.
		text := strings.ReplaceAll(ctx.Text, "\r\n", "\n")
		text = strings.ReplaceAll(text, "\n", "\r\n")
		buf.WriteString(text)
		if !strings.HasSuffix(text, "\r\n") {
			buf.WriteString("\r\n")
		}
	}
	return buf.String()
}

// BuildForwardHTMLBody composes the text/html body of a forward. The
// caller's HTML comment is emitted as-is (the API contract treats
// html_body as caller-controlled markup); the forwarded block is wrapped
// in a blockquote so mail clients render it visually as a quote.
func BuildForwardHTMLBody(commentHTML string, ctx ForwardContext) string {
	var buf strings.Builder
	if c := strings.TrimSpace(commentHTML); c != "" {
		buf.WriteString(c)
		buf.WriteString("\r\n<br><br>\r\n")
	}
	buf.WriteString(`<div class="forwarded">`)
	buf.WriteString("\r\n")
	buf.WriteString("---------- Forwarded message ---------<br>\r\n")
	writeHTMLHeaderLine(&buf, "From", ctx.From)
	writeHTMLHeaderLine(&buf, "Date", ctx.Date)
	writeHTMLHeaderLine(&buf, "Subject", ctx.Subject)
	writeHTMLHeaderLine(&buf, "To", ctx.To)
	writeHTMLHeaderLine(&buf, "Cc", ctx.Cc)
	buf.WriteString("<br>\r\n")
	if ctx.HTML != "" {
		buf.WriteString(`<blockquote style="margin:0 0 0 0.8ex;border-left:1px solid #ccc;padding-left:1ex">`)
		buf.WriteString("\r\n")
		buf.WriteString(ctx.HTML)
		buf.WriteString("\r\n</blockquote>")
	} else if ctx.Text != "" {
		buf.WriteString(`<blockquote style="margin:0 0 0 0.8ex;border-left:1px solid #ccc;padding-left:1ex"><pre>`)
		buf.WriteString(htmlEscape(ctx.Text))
		buf.WriteString("</pre></blockquote>")
	}
	buf.WriteString("\r\n</div>")
	return buf.String()
}

func writeHeaderLine(buf *strings.Builder, name, value string) {
	if value == "" {
		return
	}
	buf.WriteString(name)
	buf.WriteString(": ")
	buf.WriteString(value)
	buf.WriteString("\r\n")
}

func writeHTMLHeaderLine(buf *strings.Builder, name, value string) {
	if value == "" {
		return
	}
	buf.WriteString("<b>")
	buf.WriteString(name)
	buf.WriteString(":</b> ")
	buf.WriteString(htmlEscape(value))
	buf.WriteString("<br>\r\n")
}

// htmlEscape is a tiny subset of html.EscapeString — sufficient for
// the four characters that can break a blockquote. Avoids pulling in
// html/template for one function.
func htmlEscape(s string) string {
	if !strings.ContainsAny(s, "&<>\"") {
		return s
	}
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;").Replace(s)
}
