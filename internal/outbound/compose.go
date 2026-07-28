package outbound

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/textproto"
	"strings"
	"time"
)

// NOTE: Message-ID is intentionally omitted from composed messages.
// SES assigns its own Message-ID on send, and we capture it from the
// SMTP response. This avoids a mismatch where recipients see the SES ID
// but we tracked a different one.

// ComposeMessage builds an RFC 2822 email message (single content type).
// Message-ID is omitted — SES assigns one on send.
// If to is empty, the To: header is omitted entirely (CC-only send).
// BCC is never written to headers — it is handled at the SMTP envelope level.
// When conversationID is non-empty, an X-E2A-Conversation-ID header is written
// so recipient agents on this platform can continue the same application thread
// without depending on In-Reply-To chains.
//
// Threading headers (RFC 5322 § 3.6.4):
//   - replyToMsgID is the immediate parent's Message-ID — written to In-Reply-To.
//   - references is the FULL ancestor chain in conversation order (oldest →
//     newest, including the immediate parent). When non-empty, written as the
//     References header in space-separated form. When empty but replyToMsgID
//     is set, References falls back to [replyToMsgID] for backwards compat.
//
// Why the full chain matters: in multi-party email threads, some participants
// may not have seen every prior Message-ID (e.g. agent A replies only to
// agent B; agent B then replies-all back to user — user has no record of
// agent A's reply). Without the full References chain, the user's mail client
// (Gmail) can't anchor the reply to the existing thread and forks a new one.
// With the full chain, the client matches on ANY prior ID and threads correctly.
func ComposeMessage(from string, to []string, cc []string, subject, body, contentType, replyToMsgID string, references []string, fromDomain, replyTo, conversationID string) ([]byte, error) {
	if contentType == "" {
		contentType = "text/plain"
	}

	var buf strings.Builder
	writeHeader := headerWriter(&buf)

	writeHeader("From", from)
	if len(to) > 0 {
		writeHeader("To", strings.Join(to, ", "))
	}
	if len(cc) > 0 {
		writeHeader("Cc", strings.Join(cc, ", "))
	}
	if replyTo != "" {
		writeHeader("Reply-To", replyTo)
	}
	writeHeader("Subject", mime.QEncoding.Encode("utf-8", subject))
	writeHeader("Date", time.Now().UTC().Format(time.RFC1123Z))
	encoding, encodedBody := encodeBody(body)

	writeHeader("MIME-Version", "1.0")
	writeHeader("Content-Type", contentType+"; charset=utf-8")
	writeHeader("Content-Transfer-Encoding", encoding)

	writeThreadingHeaders(writeHeader, replyToMsgID, references)
	if conversationID != "" {
		writeHeader("X-E2A-Conversation-ID", conversationID)
	}

	buf.WriteString("\r\n")
	buf.WriteString(encodedBody)

	return []byte(buf.String()), nil
}

// ComposeMultipartMessage builds an RFC 2822 multipart/alternative email with text and HTML parts.
// If htmlBody is empty, falls back to a single text/plain message via ComposeMessage.
// See ComposeMessage for replyToMsgID / references semantics.
func ComposeMultipartMessage(from string, to []string, cc []string, subject, textBody, htmlBody, replyToMsgID string, references []string, fromDomain, replyTo, conversationID string) ([]byte, error) {
	if htmlBody == "" {
		return ComposeMessage(from, to, cc, subject, textBody, "text/plain", replyToMsgID, references, fromDomain, replyTo, conversationID)
	}

	boundary := generateBoundary()

	var buf strings.Builder
	writeHeader := headerWriter(&buf)

	writeHeader("From", from)
	if len(to) > 0 {
		writeHeader("To", strings.Join(to, ", "))
	}
	if len(cc) > 0 {
		writeHeader("Cc", strings.Join(cc, ", "))
	}
	if replyTo != "" {
		writeHeader("Reply-To", replyTo)
	}
	writeHeader("Subject", mime.QEncoding.Encode("utf-8", subject))
	writeHeader("Date", time.Now().UTC().Format(time.RFC1123Z))
	writeHeader("MIME-Version", "1.0")
	writeHeader("Content-Type", fmt.Sprintf("multipart/alternative; boundary=%q", boundary))

	writeThreadingHeaders(writeHeader, replyToMsgID, references)
	if conversationID != "" {
		writeHeader("X-E2A-Conversation-ID", conversationID)
	}

	buf.WriteString("\r\n")

	// text/plain part
	buf.WriteString("--" + boundary + "\r\n")
	writeBodyPart(&buf, "text/plain", textBody)

	// text/html part
	buf.WriteString("--" + boundary + "\r\n")
	writeBodyPart(&buf, "text/html", htmlBody)

	// closing boundary
	buf.WriteString("--" + boundary + "--\r\n")

	return []byte(buf.String()), nil
}

// ComposeMessageWithAttachments builds an RFC 2822 multipart/mixed email with attachments.
// If no attachments are provided, falls back to ComposeMultipartMessage.
// See ComposeMessage for replyToMsgID / references semantics.
func ComposeMessageWithAttachments(from string, to []string, cc []string, subject, textBody, htmlBody, replyToMsgID string, references []string, fromDomain, replyTo, conversationID string, attachments []Attachment) ([]byte, error) {
	// Defense-in-depth header-injection guard: reject any attachment
	// whose user-supplied Filename or ContentType contains CR or LF.
	// fmt.Sprintf("%q", ...) escapes Filename safely, but ContentType
	// is written via "%s" and would inject extra MIME headers if it
	// contained "\r\n" — so reject before composing.
	for _, att := range attachments {
		if strings.ContainsAny(att.Filename, "\r\n") {
			return nil, fmt.Errorf("attachment filename contains CR/LF: header injection refused")
		}
		if strings.ContainsAny(att.ContentType, "\r\n") {
			return nil, fmt.Errorf("attachment content_type contains CR/LF: header injection refused")
		}
	}
	if len(attachments) == 0 {
		return ComposeMultipartMessage(from, to, cc, subject, textBody, htmlBody, replyToMsgID, references, fromDomain, replyTo, conversationID)
	}

	mixedBoundary := generateBoundary()

	var buf strings.Builder
	writeHeader := headerWriter(&buf)

	writeHeader("From", from)
	if len(to) > 0 {
		writeHeader("To", strings.Join(to, ", "))
	}
	if len(cc) > 0 {
		writeHeader("Cc", strings.Join(cc, ", "))
	}
	if replyTo != "" {
		writeHeader("Reply-To", replyTo)
	}
	writeHeader("Subject", mime.QEncoding.Encode("utf-8", subject))
	writeHeader("Date", time.Now().UTC().Format(time.RFC1123Z))
	writeHeader("MIME-Version", "1.0")
	writeHeader("Content-Type", fmt.Sprintf("multipart/mixed; boundary=%q", mixedBoundary))

	writeThreadingHeaders(writeHeader, replyToMsgID, references)
	if conversationID != "" {
		writeHeader("X-E2A-Conversation-ID", conversationID)
	}

	buf.WriteString("\r\n")

	// Body part
	if htmlBody != "" {
		altBoundary := generateBoundary()
		buf.WriteString("--" + mixedBoundary + "\r\n")
		buf.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=%q\r\n\r\n", altBoundary))

		buf.WriteString("--" + altBoundary + "\r\n")
		writeBodyPart(&buf, "text/plain", textBody)

		buf.WriteString("--" + altBoundary + "\r\n")
		writeBodyPart(&buf, "text/html", htmlBody)

		buf.WriteString("--" + altBoundary + "--\r\n")
	} else {
		buf.WriteString("--" + mixedBoundary + "\r\n")
		writeBodyPart(&buf, "text/plain", textBody)
	}

	// Attachment parts
	for _, att := range attachments {
		buf.WriteString("--" + mixedBoundary + "\r\n")
		buf.WriteString(fmt.Sprintf("Content-Type: %s\r\n", att.ContentType))
		buf.WriteString(fmt.Sprintf("Content-Disposition: attachment; filename=%q\r\n", att.Filename))
		buf.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")

		// att.Data is already base64-encoded from the API request
		// Wrap at 76 chars per RFC 2045
		data := att.Data
		for len(data) > 76 {
			buf.WriteString(data[:76])
			buf.WriteString("\r\n")
			data = data[76:]
		}
		if len(data) > 0 {
			buf.WriteString(data)
			buf.WriteString("\r\n")
		}
	}

	buf.WriteString("--" + mixedBoundary + "--\r\n")

	return []byte(buf.String()), nil
}

// DecodeAttachmentData decodes a base64-encoded attachment data string.
func DecodeAttachmentData(data string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(data)
}

// writeThreadingHeaders writes the In-Reply-To and References headers per
// RFC 5322 § 3.6.4. References is the full ancestor chain in conversation
// order (oldest → newest) and must be space-separated message-ids; mail
// clients use it to anchor a reply to an existing thread by matching ANY
// id in the chain. When references is empty but replyToMsgID is set, the
// References header falls back to a single id (legacy behavior); use a
// non-empty references slice for any reply that may reach a recipient who
// did not see the immediate parent (multi-party / agent-mediated threads).
func writeThreadingHeaders(writeHeader func(string, string), replyToMsgID string, references []string) {
	if replyToMsgID != "" {
		writeHeader("In-Reply-To", replyToMsgID)
	}
	if len(references) > 0 {
		writeHeader("References", strings.Join(references, " "))
	} else if replyToMsgID != "" {
		writeHeader("References", replyToMsgID)
	}
}

// encodeBody picks the Content-Transfer-Encoding for one body part and
// returns the encoding name alongside the body encoded for that value.
//
// RFC 2045 § 6.1 makes "7bit" the default when no Content-Transfer-Encoding
// header is present, so writing a UTF-8 body raw and omitting the header —
// which is what this package used to do — declares 7bit while delivering
// 8-bit bytes. Receivers treat that as malformed: SpamAssassin scores it
// CTE_8BIT_MISMATCH (+1.0), because a broken transfer encoding is a common
// bulk-mailer tell. A single em dash in an otherwise clean message was
// enough to trigger it.
//
// Pure-ASCII bodies stay 7bit and byte-identical, so the common path is
// unchanged. Anything else becomes quoted-printable, which keeps the text
// human-readable on the wire, needs no 8BITMIME support from the relay, and
// is already decoded on the receive side by internal/mailparse.
func encodeBody(body string) (encoding, encoded string) {
	if isASCII(body) {
		return "7bit", body
	}
	var buf strings.Builder
	w := quotedprintable.NewWriter(&buf)
	if _, err := io.WriteString(w, body); err != nil {
		return "8bit", body
	}
	if err := w.Close(); err != nil {
		return "8bit", body
	}
	return "quoted-printable", buf.String()
}

// isASCII reports whether s is entirely 7-bit. Checked bytewise rather than
// by rune: the question is what goes on the wire, and any multi-byte UTF-8
// sequence has the high bit set on every byte.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

// writeBodyPart writes one MIME part's Content-Type, Content-Transfer-Encoding
// and encoded body, using the encoding encodeBody selected for it.
func writeBodyPart(buf *strings.Builder, contentType, body string) {
	encoding, encoded := encodeBody(body)
	buf.WriteString("Content-Type: " + contentType + "; charset=utf-8\r\n")
	buf.WriteString("Content-Transfer-Encoding: " + encoding + "\r\n\r\n")
	buf.WriteString(encoded)
	buf.WriteString("\r\n")
}

func headerWriter(buf *strings.Builder) func(string, string) {
	return func(key, value string) {
		buf.WriteString(textproto.CanonicalMIMEHeaderKey(key))
		buf.WriteString(": ")
		buf.WriteString(sanitizeHeaderValue(value))
		buf.WriteString("\r\n")
	}
}

// sanitizeHeaderValue strips CR and LF to prevent header injection.
// Without this, an attacker-controlled value like "abc\r\nBcc: leak@evil.com"
// in conversation_id (or any other passthrough header) would smuggle
// arbitrary headers into the composed message — a blind-Bcc /
// fake-DKIM-Signature primitive available to any authenticated user.
// Stripping is preferred over rejecting so the request still succeeds
// with the malicious bytes neutralised; the API layer additionally
// validates conversation_id and returns 400 on CRLF, but this is the
// last line of defense for any future caller.
func sanitizeHeaderValue(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

func generateBoundary() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure means the OS RNG is broken — nothing
		// downstream will work either. Panic so the caller surfaces a
		// 500 rather than silently emitting an all-zero boundary that
		// could collide across messages.
		panic(fmt.Sprintf("compose: crypto/rand failed: %v", err))
	}
	return fmt.Sprintf("e2a_%x", b)
}
