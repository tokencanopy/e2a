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
	"unicode"
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
	if err := ValidateAttachmentFilenames(attachments); err != nil {
		return nil, err
	}
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
		buf.WriteString(contentDispositionHeaderPrefix + attachmentDisposition(att.Filename) + "\r\n")
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
// A remote ID too long to fit on a legal header line is omitted rather than
// making the entire reply invalid at the next strict SMTP hop.
func writeThreadingHeaders(writeHeader func(string, string), replyToMsgID string, references []string) {
	if !threadingMessageIDFitsLine(replyToMsgID) {
		replyToMsgID = ""
	}
	if replyToMsgID != "" {
		writeHeader("In-Reply-To", replyToMsgID)
	}
	safeReferences := make([]string, 0, len(references))
	for _, id := range references {
		if threadingMessageIDFitsLine(id) {
			safeReferences = append(safeReferences, id)
		}
	}
	if len(safeReferences) > 0 {
		writeHeader("References", strings.Join(safeReferences, " "))
	} else if replyToMsgID != "" {
		writeHeader("References", replyToMsgID)
	}
}

// maxHeaderOctets is the RFC 5322 § 2.1.1 line limit, excluding CRLF.
// SMTP (RFC 5321 § 4.5.3.1.6) allows 1000 including CRLF, so a header
// line longer than this can be rejected outright by a strict relay.
const maxHeaderOctets = 998

// A Message-ID is an indivisible token: inserting a fold inside it would
// change the identifier. Inbound IDs are remote input and have no published
// length bound, so omit an ID that cannot fit on an In-Reply-To line. The
// References prefix is shorter, making this conservative for both fields.
func threadingMessageIDFitsLine(id string) bool {
	return id != "" && len("In-Reply-To: ")+len(id) <= maxHeaderOctets
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
// Over-length lines force the same treatment even when the body is pure
// ASCII. RFC 5322 § 2.1.1 caps a line at 998 octets, and a compliant MTA
// hard-wraps anything longer — after we have signed. Relaxed DKIM body
// canonicalisation absorbs *rewritten* line endings (bare LF to CRLF is
// harmless) but not *inserted* ones, so a wrap mid-line changes the body
// hash and the signature fails with "body hash did not verify". Routing
// those bodies through quoted-printable, which soft-wraps at 76 octets,
// means no downstream wrap is ever needed.
//
// Otherwise transport-safe ASCII bodies stay 7bit and byte-identical. NUL and
// a lone CR are not valid 7bit data even though their bytes are ASCII, so they
// take the same quoted-printable path as non-ASCII data. Bare LF remains on
// the fast path because net/textproto.DotWriter canonicalises it to CRLF
// before SMTP transmission, and preserving it here keeps the stored composed
// body byte-identical. Quoted-printable keeps the text human-readable on the
// wire, needs no 8BITMIME support from the relay, and is already decoded on
// the receive side by internal/mailparse.
func encodeBody(body string) (encoding, encoded string) {
	if is7BitTransportSafe(body) && !hasOverlongLine(body) {
		return "7bit", body
	}
	body = canonicalizeLineEndings(body)
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

// canonicalizeLineEndings converts every CRLF, lone CR, and bare LF line
// ending to CRLF before quoted-printable encoding. Go's text-mode QP writer
// tracks a pending CR internally; feeding it an encoded byte between CR and LF
// can otherwise make it mistake the later LF for the CR's partner and drop it.
func canonicalizeLineEndings(s string) string {
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(s)
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// maxLineOctets is the RFC 5322 § 2.1.1 limit on a line's content,
// excluding the trailing CRLF.
const maxLineOctets = 998

// hasOverlongLine reports whether any line in s exceeds maxLineOctets.
// Lines are split on LF and a trailing CR is discounted, so the check is
// correct whether the caller supplied CRLF or bare LF endings.
func hasOverlongLine(s string) bool {
	for {
		line, rest, more := strings.Cut(s, "\n")
		if len(strings.TrimSuffix(line, "\r")) > maxLineOctets {
			return true
		}
		if !more {
			return false
		}
		s = rest
	}
}

// is7BitTransportSafe reports whether s can safely take the 7bit path apart
// from the line-length constraint checked separately by hasOverlongLine.
// NUL is forbidden, as is a CR not followed by LF. Bare LF is accepted here
// because the SMTP dot writer canonicalises it on the wire.
func is7BitTransportSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case 0:
			return false
		case '\r':
			if i+1 >= len(s) || s[i+1] != '\n' {
				return false
			}
			i++ // consume the LF in this CRLF pair
		case '\n':
			continue
		default:
			if s[i] > 127 {
				return false
			}
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

const contentDispositionHeaderPrefix = "Content-Disposition: "

// attachmentDisposition renders the Content-Disposition header value for an
// attachment filename.
//
// MIME parameter values are ASCII-only. A non-ASCII filename must be
// encoded per RFC 2231 as filename*=utf-8''<percent-encoded>, otherwise the
// raw UTF-8 bytes land in a header field — an 8-bit value in a place that
// only permits 7-bit, which receivers are free to mangle or reject.
// mime.FormatMediaType applies that encoding, and quotes or escapes values
// that merely contain specials, so it handles both cases.
//
// It returns "" for a value it cannot represent at all; fall back to the
// historical quoted form rather than emitting a part with no disposition,
// which would change how the attachment is presented. Callers have already
// rejected CR/LF in the filename, so the fallback cannot inject headers.
//
// Pure-ASCII names keep the historical %q form so the common case stays
// byte-identical on the wire. FormatMediaType would emit them unquoted —
// equally valid, but a gratuitous change to every message with an
// attachment when only the non-ASCII case is broken.
//
// A long encoded value uses RFC 2231 continuation parameters. Percent-encoding
// can triple the wire size, so a filename within the API's byte cap can still
// make a single filename*= parameter exceed SMTP's 998-octet line limit.
// Continuations keep each parameter on a short foldable line and reconstruct
// byte-for-byte.
func attachmentDisposition(filename string) string {
	var disposition string
	nonASCII := strings.IndexFunc(filename, func(r rune) bool { return r > unicode.MaxASCII }) >= 0
	if nonASCII {
		if d := mime.FormatMediaType("attachment", map[string]string{"filename": filename}); d != "" {
			disposition = d
		}
	}
	if disposition == "" {
		disposition = fmt.Sprintf("attachment; filename=%q", filename)
	}
	if len(contentDispositionHeaderPrefix)+len(disposition) <= maxLineOctets {
		return disposition
	}
	return continuedAttachmentDisposition(filename)
}

func continuedAttachmentDisposition(filename string) string {
	const bytesPerSegment = 20
	const upperhex = "0123456789ABCDEF"

	var disposition strings.Builder
	disposition.Grow(len(filename)*3 + len(filename)/bytesPerSegment*20)
	disposition.WriteString("attachment")

	for segment, offset := 0, 0; offset < len(filename); segment++ {
		end := min(offset+bytesPerSegment, len(filename))
		disposition.WriteString(";\r\n filename*")
		disposition.WriteString(fmt.Sprintf("%d*=", segment))
		if segment == 0 {
			disposition.WriteString("utf-8''")
		}
		for _, b := range []byte(filename[offset:end]) {
			disposition.WriteByte('%')
			disposition.WriteByte(upperhex[b>>4])
			disposition.WriteByte(upperhex[b&0x0f])
		}
		offset = end
	}
	return disposition.String()
}

// foldTarget is the length a folded header aims for. RFC 5322 § 2.1.1
// recommends 78 including CRLF; a fold is only triggered by exceeding
// maxHeaderOctets, so this governs the shape of an already-broken header
// rather than reformatting ordinary ones.
const foldTarget = 76

func headerWriter(buf *strings.Builder) func(string, string) {
	return func(key, value string) {
		line := textproto.CanonicalMIMEHeaderKey(key) + ": " + sanitizeHeaderValue(value)
		buf.WriteString(foldHeaderLine(line))
		buf.WriteString("\r\n")
	}
}

// foldHeaderLine breaks an over-length header field into continuation
// lines per RFC 5322 § 2.2.3, each beginning with a single space.
//
// Nothing was folding before, so a long Q-encoded Subject or a deep
// References chain went out as one line — measured at over 3200 octets
// for a long non-ASCII subject. That is past both the RFC 5322 limit and
// the SMTP line limit, and a strict relay may refuse the message.
//
// Only lines past maxHeaderOctets are touched, so ordinary headers are
// byte-identical to before. This is deliberately not a reformat of every
// header: shortening lines that already fit would change the wire output
// of essentially every message to fix a case that only arises at the
// extremes.
//
// Folding is safe for DKIM regardless of whether it happens here or at a
// relay: relaxed header canonicalisation (RFC 6376 § 3.4.2) unfolds and
// collapses whitespace before hashing. Composition also runs before
// signing, so the signature covers the folded form either way.
func foldHeaderLine(line string) string {
	if len(line) <= maxHeaderOctets {
		return line
	}

	var out strings.Builder
	out.Grow(len(line) + len(line)/foldTarget*3)

	// Never fold at the space directly after the field name: it moves the
	// whole value to a continuation line without shortening anything, so a
	// single unfoldable token would gain useless structure and stay over
	// the limit anyway. Requiring fold points past the name means such a
	// value is returned verbatim instead.
	minFold := strings.Index(line, ": ") + 2

	cur := 0        // octets on the current output line
	lastSpace := -1 // index in the current line's buffer of the last foldable space
	var pending strings.Builder
	inQuotes := false

	flush := func(upTo int) {
		s := pending.String()
		out.WriteString(s[:upTo])
		out.WriteString("\r\n ")
		rest := strings.TrimPrefix(s[upTo:], " ")
		pending.Reset()
		pending.WriteString(rest)
		cur = len(rest) + 1
		lastSpace = -1
	}

	for i := 0; i < len(line); i++ {
		c := line[i]
		// Track quoted strings so a fold never lands inside one, where a
		// receiver would read the inserted CRLF+SPACE as content.
		if c == '"' && (i == 0 || line[i-1] != '\\') {
			inQuotes = !inQuotes
		}
		if c == ' ' && !inQuotes && i > minFold {
			lastSpace = pending.Len()
		}
		pending.WriteByte(c)
		cur++

		if cur > foldTarget && lastSpace > 0 {
			flush(lastSpace)
		}
	}

	out.WriteString(pending.String())
	return out.String()
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
