package outbound

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// MaxComposedMessageBytes is the hard cap on a composed outbound message — the
// sum of subject + text + html + DECODED attachment bytes. It is the SES v1
// stored-message ceiling (the real upstream limit), distinct from the per-field
// (subject/body) and per-attachment caps: a caller can stay under every
// individual limit while the composed MIME still exceeds what the upstream
// provider accepts, so the true ceiling is checked on the fully-composed
// content. Over → 413 payload_too_large at the API edge.
const MaxComposedMessageBytes = 10 * 1024 * 1024

// MaxAttachmentFilenameBytes bounds user-controlled MIME parameter expansion.
// RFC 2231 percent-encoding can triple the filename's byte length; without a
// separate bound, a small attachment with a multi-megabyte filename can pass
// decoded-content limits and force disproportionately large allocations during
// composition.
const MaxAttachmentFilenameBytes = 1024

// ValidateAttachmentFilenames applies the shared composition-time filename
// bound before any RFC 2231 encoding or MIME allocation takes place.
func ValidateAttachmentFilenames(atts []Attachment) error {
	for i, att := range atts {
		if len(att.Filename) > MaxAttachmentFilenameBytes {
			return &ValidationError{Message: fmt.Sprintf(
				"attachment %d filename is %d bytes; maximum is %d bytes",
				i, len(att.Filename), MaxAttachmentFilenameBytes,
			)}
		}
	}
	return nil
}

// ComposedSizeError reports the final, post-feature-composition size. It is
// distinct from ordinary request validation so API layers can preserve the
// canonical 413 payload_too_large contract.
type ComposedSizeError struct {
	ActualBytes int
	MaxBytes    int
}

func (e *ComposedSizeError) Error() string {
	return fmt.Sprintf("composed message is %d bytes; maximum is %d bytes", e.ActualBytes, e.MaxBytes)
}

func IsComposedSizeError(err error) bool {
	var target *ComposedSizeError
	return errors.As(err, &target)
}

// ComposedSize returns the composed byte total of an outbound message: the sum
// of subject + text + html + DECODED attachment bytes. Attachment Data is
// base64; embedded whitespace (CR/LF, spaces, tabs) is stripped before decoding
// and a decode failure falls back to the raw wire length so the total is never
// under-counted.
//
// This is the single source of truth for the composed-message ceiling, shared
// by the direct send path (httpapi send/reply/forward) and the HITL approve-
// override path (agent) so neither entry point can bypass the cap.
func ComposedSize(subject, text, html string, atts []Attachment) int {
	total := len(subject) + len(text) + len(html)
	for _, att := range atts {
		clean := strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, att.Data)
		decoded, err := base64.StdEncoding.DecodeString(clean)
		if err != nil {
			total += len(att.Data) // never under-count on a decode miss
			continue
		}
		total += len(decoded)
	}
	return total
}
