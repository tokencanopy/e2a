package mailparse

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// A multipart/mixed message: text body + a base64 PDF attachment + a named
// inline image. The two named parts are attachments; the text body is not.
func sampleMultipart() []byte {
	return []byte("From: a@x.com\r\n" +
		"To: b@y.com\r\n" +
		"Subject: hi\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=BOUND\r\n" +
		"\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"the body text\r\n" +
		"--BOUND\r\n" +
		"Content-Type: application/pdf; name=\"report.pdf\"\r\n" +
		"Content-Disposition: attachment; filename=\"report.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" + b64("%PDF-1.4 fake pdf bytes") + "\r\n" +
		"--BOUND\r\n" +
		"Content-Type: image/png\r\n" +
		"Content-Disposition: inline; filename=\"logo.png\"\r\n" +
		"Content-ID: <ii_logo@mail.gmail.com>\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" + b64("\x89PNG\r\n fake png") + "\r\n" +
		"--BOUND--\r\n")
}

func TestAttachments_OrderAndDecode(t *testing.T) {
	atts := Attachments(sampleMultipart())
	if len(atts) != 2 {
		t.Fatalf("want 2 attachments (text body excluded), got %d: %+v", len(atts), atts)
	}
	if atts[0].Filename != "report.pdf" || atts[0].ContentType != "application/pdf" {
		t.Errorf("att0 meta wrong: %+v", atts[0])
	}
	if string(atts[0].Data) != "%PDF-1.4 fake pdf bytes" {
		t.Errorf("att0 bytes not decoded: %q", atts[0].Data)
	}
	if atts[1].Filename != "logo.png" || atts[1].ContentType != "image/png" {
		t.Errorf("att1 meta wrong: %+v", atts[1])
	}
	if !bytes.Equal(atts[1].Data, []byte("\x89PNG\r\n fake png")) {
		t.Errorf("att1 binary bytes not decoded: %q", atts[1].Data)
	}
}

func TestAttachmentsBase64Padding(t *testing.T) {
	for name, data := range map[string][]byte{
		"two padding characters": {0x00, 0xff, 0x80, 0xfb},
		"one padding character":  {0x00, 0xff, 0x80, 0xfb, 0xff},
	} {
		for encodingName, encoding := range map[string]*base64.Encoding{
			"padded":   base64.StdEncoding,
			"unpadded": base64.RawStdEncoding,
		} {
			t.Run(name+"/"+encodingName, func(t *testing.T) {
				encoded := encoding.EncodeToString(data)
				encoded = encoded[:4] + " \t\r\n" + encoded[4:]
				raw := []byte("Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
					"--B\r\nContent-Type: text/plain\r\n\r\nBody\r\n" +
					"--B\r\nContent-Type: application/octet-stream\r\n" +
					"Content-Disposition: attachment; filename=\"sample.bin\"\r\n" +
					"Content-Transfer-Encoding: base64\r\n\r\n" + encoded + "\r\n--B--\r\n")
				atts := Attachments(raw)
				if len(atts) != 1 {
					t.Fatalf("want 1 attachment, got %d", len(atts))
				}
				if !bytes.Equal(atts[0].Data, data) {
					t.Errorf("attachment bytes = %x, want %x", atts[0].Data, data)
				}
			})
		}
	}
}

// The inline image's Content-ID is captured (angle brackets stripped) so a
// renderer can resolve an HTML `cid:` reference to it; the ordinary PDF
// attachment carries none.
func TestAttachments_ContentID(t *testing.T) {
	atts := Attachments(sampleMultipart())
	if len(atts) != 2 {
		t.Fatalf("want 2 attachments, got %d", len(atts))
	}
	if atts[0].ContentID != "" {
		t.Errorf("PDF attachment should have no Content-ID, got %q", atts[0].ContentID)
	}
	if atts[1].ContentID != "ii_logo@mail.gmail.com" {
		t.Errorf("inline image Content-ID want %q, got %q", "ii_logo@mail.gmail.com", atts[1].ContentID)
	}
}

// An inline image with a Content-ID but NO filename and no attachment
// disposition is still included (it's a fetchable cid: resource), while a
// text/html body part carrying a Content-ID (a multipart/related root) stays a
// body and is NOT treated as an attachment.
func TestAttachments_CidOnlyInlineIncludedButBodyExcluded(t *testing.T) {
	png := b64("\x89PNG cid-only")
	raw := []byte("MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/related; boundary=R\r\n\r\n" +
		"--R\r\n" +
		"Content-Type: text/html\r\n" +
		"Content-ID: <root.html>\r\n" +
		"\r\n<p><img src=\"cid:logo\"></p>\r\n" +
		"--R\r\n" +
		"Content-Type: image/png\r\n" +
		"Content-ID: <logo>\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" + png + "\r\n" +
		"--R--\r\n")
	atts := Attachments(raw)
	if len(atts) != 1 {
		t.Fatalf("want 1 attachment (the cid-only image; the html body excluded), got %d: %+v", len(atts), atts)
	}
	if atts[0].ContentID != "logo" || atts[0].ContentType != "image/png" || atts[0].Filename != "" {
		t.Errorf("cid-only inline image meta wrong: %+v", atts[0])
	}
}

func TestTrimContentID(t *testing.T) {
	cases := map[string]string{
		"<ii_abc@mail.gmail.com>": "ii_abc@mail.gmail.com",
		"  <ii_abc>  ":            "ii_abc",
		"ii_bare":                 "ii_bare",
		"":                        "",
		"   ":                     "",
	}
	for in, want := range cases {
		if got := trimContentID(in); got != want {
			t.Errorf("trimContentID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAttachmentAt_Bounds(t *testing.T) {
	raw := sampleMultipart()
	if _, ok := AttachmentAt(raw, 0); !ok {
		t.Error("index 0 should exist")
	}
	if a, ok := AttachmentAt(raw, 1); !ok || a.Filename != "logo.png" {
		t.Errorf("index 1 should be logo.png, got ok=%v %+v", ok, a)
	}
	if _, ok := AttachmentAt(raw, 2); ok {
		t.Error("index 2 is out of range, want ok=false")
	}
	if _, ok := AttachmentAt(raw, -1); ok {
		t.Error("negative index, want ok=false")
	}
}

func TestAttachments_NoneOnPlainMessage(t *testing.T) {
	plain := []byte("From: a@x.com\r\nSubject: hi\r\nContent-Type: text/plain\r\n\r\njust text, no attachments\r\n")
	if atts := Attachments(plain); len(atts) != 0 {
		t.Errorf("plain message should have no attachments, got %d", len(atts))
	}
}

func TestAttachments_QuotedPrintable(t *testing.T) {
	raw := []byte("Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain; name=\"note.txt\"\r\n" +
		"Content-Disposition: attachment; filename=\"note.txt\"\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\nca=C3=A9\r\n" + // "caé"
		"--B--\r\n")
	atts := Attachments(raw)
	if len(atts) != 1 || string(atts[0].Data) != "caé" {
		t.Fatalf("quoted-printable attachment decode failed: %+v", atts)
	}
}

func TestAttachments_MalformedReturnsEmpty(t *testing.T) {
	if atts := Attachments([]byte("not a real \x00 mime message")); atts != nil && len(atts) != 0 {
		t.Errorf("malformed input should yield no attachments, got %+v", atts)
	}
}

// Nested multipart (mixed → related): index must stay stable depth-first across
// nesting, and a part with `Content-Disposition: attachment` but NO filename must
// still be collected (the isAttachmentDisp branch).
func TestAttachments_NestedMultipartAndUnnamedAttachment(t *testing.T) {
	raw := []byte("Content-Type: multipart/mixed; boundary=OUT\r\n\r\n" +
		"--OUT\r\nContent-Type: multipart/related; boundary=IN\r\n\r\n" +
		"--IN\r\nContent-Type: text/html\r\n\r\n<p>hi</p>\r\n" +
		"--IN\r\nContent-Type: image/png\r\nContent-Disposition: inline; filename=\"a.png\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" + b64("PNGA") + "\r\n" +
		"--IN--\r\n" +
		"--OUT\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment\r\nContent-Transfer-Encoding: base64\r\n\r\n" + b64("rawbytes") + "\r\n" +
		"--OUT--\r\n")
	atts := Attachments(raw)
	if len(atts) != 2 {
		t.Fatalf("want 2 attachments across nesting (html body excluded), got %d: %+v", len(atts), atts)
	}
	// Depth-first: the nested a.png comes before the top-level unnamed attachment.
	if atts[0].Filename != "a.png" || string(atts[0].Data) != "PNGA" {
		t.Errorf("att0 (nested) wrong: %+v", atts[0])
	}
	// Unnamed attachment-disposition part is collected (filename empty).
	if atts[1].Filename != "" || atts[1].ContentType != "application/octet-stream" || string(atts[1].Data) != "rawbytes" {
		t.Errorf("att1 (unnamed attachment) wrong: %+v", atts[1])
	}
}
