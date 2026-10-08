package mailparse

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
)

// Attachment is one decoded attachment part of a MIME message.
type Attachment struct {
	Filename    string // decoded filename (RFC 2047/2231), "" if the part has none
	ContentType string // media type only (e.g. "application/pdf"), params stripped
	// ContentID is the part's Content-ID with the surrounding angle brackets
	// stripped (e.g. "ii_abc@mail.gmail.com"), or "" if the part carries none.
	// This is the key an HTML body's `cid:` reference resolves against, so a
	// renderer can map `<img src="cid:ii_abc@mail.gmail.com">` to this
	// attachment. Present mainly on inline images embedded by mail clients.
	ContentID string
	Data      []byte // decoded bytes (Content-Transfer-Encoding applied)
}

// Attachments walks the MIME tree of a raw RFC 5322 message and returns its
// attachment parts in stable document order (depth-first, as they appear in the
// wire bytes). Index N from this function is the SAME index used by AttachmentAt
// and by the message read view — it is the authoritative attachment index.
//
// An "attachment" is a leaf part that carries a filename (in Content-Disposition
// or the Content-Type name param) OR an explicit `Content-Disposition: attachment`.
// Body parts (text/plain, text/html with no filename) and multipart containers
// are NOT attachments. Named inline parts (e.g. cid: images) ARE included — they
// are real fetchable bytes.
//
// On a parse failure or a non-multipart message with no attachment part, returns
// an empty slice (never nil-panics).
func Attachments(raw []byte) []Attachment {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	var out []Attachment
	collectAttachments(
		msg.Header.Get("Content-Type"),
		msg.Header.Get("Content-Transfer-Encoding"),
		msg.Header.Get("Content-Disposition"),
		msg.Header.Get("Content-ID"),
		"", // top-level has no part filename
		msg.Body,
		0,
		&out,
	)
	return out
}

// AttachmentAt returns the attachment at the 0-based index, or false if out of
// range. It re-walks the message (the caller holds the raw bytes); attachments
// are typically few, so a per-call walk is fine.
func AttachmentAt(raw []byte, index int) (Attachment, bool) {
	atts := Attachments(raw)
	if index < 0 || index >= len(atts) {
		return Attachment{}, false
	}
	return atts[index], true
}

// collectAttachments appends attachment leaves found under this part to *out.
func collectAttachments(contentType, cte, disposition, contentID, partFilename string, body io.Reader, depth int, out *[]Attachment) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = "text/plain" // missing/invalid Content-Type → body text, not an attachment
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		if depth >= maxMIMEDepth {
			return
		}
		boundary := params["boundary"]
		if boundary == "" {
			return
		}
		mr := multipart.NewReader(body, boundary)
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			collectAttachments(
				part.Header.Get("Content-Type"),
				part.Header.Get("Content-Transfer-Encoding"),
				part.Header.Get("Content-Disposition"),
				part.Header.Get("Content-ID"),
				part.FileName(), // decoded by mime/multipart
				part,
				depth+1,
				out,
			)
			part.Close()
		}
		return
	}

	// Leaf part. It's an attachment iff it has a filename or is explicitly an
	// attachment disposition.
	filename := partFilename
	if filename == "" {
		filename = params["name"] // Content-Type: ...; name="x" (legacy)
		if filename != "" {
			if dec, derr := (&mime.WordDecoder{}).DecodeHeader(filename); derr == nil {
				filename = dec
			}
		}
	}
	isAttachmentDisp := strings.HasPrefix(strings.ToLower(strings.TrimSpace(disposition)), "attachment")
	cid := trimContentID(contentID)
	// A part with a Content-ID but no filename is still a fetchable inline
	// resource an HTML `cid:` reference resolves to (some clients embed images
	// this way) — include it so the renderer can resolve it. Guard against the
	// message's own body parts: a text/plain or text/html part can carry a
	// Content-ID (a multipart/related root) and must stay a body, never an
	// attachment.
	isTextBody := mediaType == "text/plain" || mediaType == "text/html"
	isNamedInline := cid != "" && !isTextBody
	if filename == "" && !isAttachmentDisp && !isNamedInline {
		return // body text / unnamed inline part — not a fetchable attachment
	}

	*out = append(*out, Attachment{
		Filename:    filename,
		ContentType: mediaType,
		ContentID:   cid,
		Data:        decodeBytes(body, cte),
	})
}

// trimContentID normalizes a raw Content-ID header value to the token an HTML
// `cid:` URL references: surrounding angle brackets and whitespace stripped
// (e.g. "<ii_abc@mail.gmail.com>" → "ii_abc@mail.gmail.com"). Returns "" for an
// empty/blank header.
func trimContentID(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	return strings.TrimSpace(s)
}

// decodeBytes applies Content-Transfer-Encoding for both text bodies and
// attachments, preserving the original bytes if base64 decoding fails.
func decodeBytes(body io.Reader, cte string) []byte {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		raw, _ := io.ReadAll(body)
		// Be lenient with MIME whitespace and missing padding, as in piguard.
		clean := strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, string(raw))
		if dec, err := base64.StdEncoding.DecodeString(clean); err == nil {
			return dec
		}
		if dec, err := base64.RawStdEncoding.DecodeString(clean); err == nil {
			return dec
		}
		return raw // not valid base64 — hand back what we have
	case "quoted-printable":
		b, _ := io.ReadAll(quotedprintable.NewReader(body))
		return b
	default:
		b, _ := io.ReadAll(body)
		return b
	}
}
