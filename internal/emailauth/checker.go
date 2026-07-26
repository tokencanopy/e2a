package emailauth

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/mail"
	"strings"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/dkim"
)

var defaultDMARCEvaluator = newDMARCEvaluator(netTXTResolver{resolver: net.DefaultResolver})

// CheckStatus is retained while legacy callers migrate to Status.
type CheckStatus = Status

// CheckResult holds the outcome of a single authentication check.
type CheckResult struct {
	Status CheckStatus `json:"status"`
	Detail string      `json:"detail,omitempty"`
}

// AuthVerdict aggregates SPF, DKIM, and DMARC check results. It is the inbound
// trust primitive surfaced as `auth: {spf,dkim,dmarc}` on inbound messages and
// (Slice 7) enforced on by the inbound policy.
type AuthVerdict struct {
	SPF   CheckResult `json:"spf"`
	DKIM  CheckResult `json:"dkim"`
	DMARC CheckResult `json:"dmarc"`
}

// DomainAuthenticated returns true if at least one domain-level check passed.
func (r *AuthVerdict) DomainAuthenticated() bool {
	return r.SPF.Status == StatusPass || r.DKIM.Status == StatusPass
}

// Summary returns a short string describing the results, suitable for an auth header.
func (r *AuthVerdict) Summary() string {
	parts := []string{
		fmt.Sprintf("spf=%s", r.SPF.Status),
		fmt.Sprintf("dkim=%s", r.DKIM.Status),
		fmt.Sprintf("dmarc=%s", r.DMARC.Status),
	}
	return strings.Join(parts, "; ")
}

// Check is the compatibility entry point used while relay and persistence
// callers migrate to Authentication. New code should call CheckAuthentication.
// remoteIP is the IP address of the SMTP client that connected to us.
// senderEmail is the envelope sender (MAIL FROM).
// rawMessage is the full RFC 2822 message including headers.
func Check(remoteIP net.IP, senderEmail string, rawMessage []byte) *AuthVerdict {
	authentication := CheckAuthentication(context.Background(), remoteIP, senderEmail, rawMessage)
	return &AuthVerdict{
		SPF:   CheckResult{Status: authentication.SPF.Status, Detail: authentication.SPF.Detail},
		DKIM:  aggregateLegacyDKIM(authentication.DKIM),
		DMARC: CheckResult{Status: authentication.DMARC.Status, Detail: authentication.DMARC.Detail},
	}
}

// CheckAuthentication evaluates and retains the complete SPF, DKIM, and DMARC
// evidence for an inbound message.
func CheckAuthentication(ctx context.Context, remoteIP net.IP, envelopeFrom string, rawMessage []byte) *Authentication {
	return CheckAuthenticationWithHELO(ctx, remoteIP, envelopeFrom, "", rawMessage)
}

// CheckAuthenticationWithHELO evaluates authentication with the SMTP greeting
// retained for RFC 7208 null-reverse-path SPF processing.
func CheckAuthenticationWithHELO(ctx context.Context, remoteIP net.IP, envelopeFrom, heloDomain string, rawMessage []byte) *Authentication {
	return CheckAuthenticationForAuthorWithHELO(ctx, remoteIP, envelopeFrom, heloDomain, rawMessage, ParseAuthorIdentity(rawMessage))
}

// CheckAuthenticationForAuthor evaluates authentication using an AuthorIdentity
// that was parsed once by the caller. This keeps the header_from projection and
// DMARC decision on the same security-critical interpretation of From.
func CheckAuthenticationForAuthor(ctx context.Context, remoteIP net.IP, envelopeFrom string, rawMessage []byte, author AuthorIdentity) *Authentication {
	return CheckAuthenticationForAuthorWithHELO(ctx, remoteIP, envelopeFrom, "", rawMessage, author)
}

// CheckAuthenticationForAuthorWithHELO is CheckAuthenticationForAuthor with
// the connection's SMTP HELO/EHLO identity for null-reverse-path SPF.
func CheckAuthenticationForAuthorWithHELO(ctx context.Context, remoteIP net.IP, envelopeFrom, heloDomain string, rawMessage []byte, author AuthorIdentity) *Authentication {
	return checkWithEvaluatorForAuthor(ctx, defaultDMARCEvaluator, remoteIP, envelopeFrom, heloDomain, rawMessage, author)
}

func checkWithResolver(ctx context.Context, resolver TXTResolver, remoteIP net.IP, envelopeFrom string, rawMessage []byte) *Authentication {
	return checkWithEvaluator(ctx, newDMARCEvaluator(resolver), remoteIP, envelopeFrom, "", rawMessage)
}

func checkWithEvaluator(ctx context.Context, evaluator *dmarcEvaluator, remoteIP net.IP, envelopeFrom, heloDomain string, rawMessage []byte) *Authentication {
	return checkWithEvaluatorForAuthor(ctx, evaluator, remoteIP, envelopeFrom, heloDomain, rawMessage, ParseAuthorIdentity(rawMessage))
}

func checkWithEvaluatorForAuthor(ctx context.Context, evaluator *dmarcEvaluator, remoteIP net.IP, envelopeFrom, heloDomain string, rawMessage []byte, author AuthorIdentity) *Authentication {
	authentication := &Authentication{
		SPF:  checkSPF(remoteIP, envelopeFrom, heloDomain),
		DKIM: checkDKIM(rawMessage),
	}
	evaluator.evaluateAuthentication(ctx, author.Domain, authentication)
	// No log line here: the relay (the only production caller, via
	// srv.authenticate) logs the same evidence with the message trace ID and
	// redacted sender/IP — a second copy here would just duplicate PII handling.
	return authentication

}

func aggregateLegacyDKIM(results []DKIMResult) CheckResult {
	if len(results) == 0 {
		return CheckResult{Status: StatusNone, Detail: "no DKIM signature found"}
	}
	for _, result := range results {
		if result.Status == StatusPass {
			return CheckResult{Status: result.Status, Detail: result.Detail}
		}
	}
	return CheckResult{Status: results[0].Status, Detail: results[0].Detail}
}

func normDomain(d string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
}

type AuthorIdentity struct {
	Address string
	Domain  string
}

// ParseAuthorIdentity returns the single RFC 5322 author mailbox used by both
// public header_from projection and DMARC evaluation. Ambiguous From fields
// fail closed: neither repeated fields nor a multi-address field identifies a
// single author mailbox.
func ParseAuthorIdentity(rawMessage []byte) AuthorIdentity {
	msg, err := mail.ReadMessage(bytes.NewReader(rawMessage))
	if err != nil {
		return AuthorIdentity{}
	}
	var fields []string
	for name, values := range msg.Header {
		if strings.EqualFold(name, "From") {
			fields = append(fields, values...)
		}
	}
	if len(fields) != 1 {
		return AuthorIdentity{}
	}
	addresses, err := mail.ParseAddressList(fields[0])
	if err != nil || len(addresses) != 1 {
		return AuthorIdentity{}
	}
	domain := extractDomain(addresses[0].Address)
	if domain == "" {
		return AuthorIdentity{}
	}
	return AuthorIdentity{Address: addresses[0].Address, Domain: domain}
}

func checkSPF(remoteIP net.IP, envelopeFrom, heloDomain string) SPFResult {
	if remoteIP == nil {
		return SPFResult{Status: StatusNone, Detail: "no remote IP available"}
	}

	senderEmail := spfSenderIdentity(envelopeFrom, heloDomain)
	domain := extractDomain(senderEmail)
	if domain == "" {
		if strings.TrimSpace(envelopeFrom) != "" {
			return SPFResult{Status: StatusPermError, Detail: "malformed MAIL FROM identity"}
		}
		return SPFResult{Status: StatusNone, Detail: "no usable MAIL FROM or HELO identity"}
	}

	result, err := spf.CheckHostWithSender(remoteIP, domain, senderEmail)
	return mapSPFResult(result, err, domain)
}

func spfSenderIdentity(envelopeFrom, heloDomain string) string {
	if strings.TrimSpace(envelopeFrom) != "" {
		return envelopeFrom
	}
	heloDomain = normDomain(heloDomain)
	if !validDomainName(heloDomain) || !strings.Contains(heloDomain, ".") {
		return ""
	}
	return "postmaster@" + heloDomain
}

func mapSPFResult(result spf.Result, err error, domain string) SPFResult {
	domain = normDomain(domain)
	mapped := SPFResult{Domain: stringPtr(domain)}
	switch result {
	case spf.Pass:
		mapped.Status, mapped.Detail = StatusPass, fmt.Sprintf("domain %s", domain)
	case spf.Fail:
		mapped.Status, mapped.Detail = StatusFail, fmt.Sprintf("domain %s", domain)
	case spf.SoftFail:
		mapped.Status, mapped.Detail = StatusSoftFail, fmt.Sprintf("domain %s", domain)
	case spf.Neutral:
		mapped.Status, mapped.Detail = StatusNeutral, fmt.Sprintf("domain %s", domain)
	case spf.None:
		mapped.Status, mapped.Detail = StatusNone, fmt.Sprintf("no SPF record for %s", domain)
	case spf.TempError:
		mapped.Status, mapped.Detail = StatusTempError, "temporary DNS error"
	case spf.PermError:
		mapped.Status, mapped.Detail = StatusPermError, "SPF record could not be evaluated"
	default:
		mapped.Status, mapped.Detail = StatusPermError, "unknown SPF result"
	}
	if err != nil {
		detail := fmt.Sprintf("domain %s", domain)
		if mapped.Detail != "" {
			detail = mapped.Detail
		}
		mapped.Detail = detail + ": " + err.Error()
	}
	return mapped
}

// checkDKIM returns one result per DKIM-Signature field. A body-length-limited
// signature is refused individually so it cannot invalidate an independent,
// safe signature on the same message.
func checkDKIM(rawMessage []byte) []DKIMResult {
	tags := dkimSignatureTags(rawMessage)
	verifications, err := dkim.Verify(bytes.NewReader(rawMessage))
	return mapDKIMResults(verifications, tags, err)
}

func mapDKIMResults(verifications []*dkim.Verification, tags []map[string]string, verifyErr error) []DKIMResult {
	count := len(verifications)
	if len(tags) > count {
		count = len(tags)
	}
	if len(tags) != len(verifications) {
		results := make([]DKIMResult, 0, count)
		for i := 0; i < count; i++ {
			result := DKIMResult{
				Status: StatusPermError,
				Detail: "DKIM signatures could not be correlated with parsed signature metadata",
			}
			if i < len(verifications) && verifications[i] != nil && verifications[i].Domain != "" {
				result.Domain = stringPtr(normDomain(verifications[i].Domain))
			}
			if i < len(tags) {
				if result.Domain == nil {
					if domain := normDomain(tags[i]["d"]); domain != "" {
						result.Domain = stringPtr(domain)
					}
				}
				if selector := strings.TrimSpace(tags[i]["s"]); selector != "" {
					result.Selector = stringPtr(selector)
				}
			}
			results = append(results, result)
		}
		return results
	}
	results := make([]DKIMResult, 0, count)
	for i := 0; i < count; i++ {
		var verification *dkim.Verification
		if i < len(verifications) {
			verification = verifications[i]
		}
		var signatureTags map[string]string
		if i < len(tags) {
			signatureTags = tags[i]
		}
		result := DKIMResult{}
		if domain := normDomain(signatureTags["d"]); domain != "" {
			result.Domain = stringPtr(domain)
		}
		if selector := strings.TrimSpace(signatureTags["s"]); selector != "" {
			result.Selector = stringPtr(selector)
		}
		if verification != nil && verification.Domain != "" {
			result.Domain = stringPtr(normDomain(verification.Domain))
		}
		if _, limited := signatureTags["l"]; limited {
			result.Status = StatusPolicy
			result.Detail = "DKIM-Signature includes l= body-length tag (refused: would allow tail-content injection)"
			results = append(results, result)
			continue
		}
		resultErr := verifyErr
		if verification != nil {
			resultErr = verification.Err
		}
		switch {
		case resultErr == nil && verification != nil:
			result.Status = StatusPass
			if result.Domain != nil {
				result.Detail = "domain " + *result.Domain
			}
		case dkim.IsTempFail(resultErr):
			result.Status, result.Detail = StatusTempError, resultErr.Error()
		case dkim.IsPermFail(resultErr):
			result.Status, result.Detail = StatusPermError, resultErr.Error()
		default:
			result.Status = StatusFail
			if resultErr != nil {
				result.Detail = resultErr.Error()
			} else {
				result.Detail = "DKIM signature could not be evaluated"
			}
		}
		results = append(results, result)
	}
	return results
}

// dkimSignatureHasBodyLengthTag reports whether any DKIM-Signature
// header in rawMessage carries an `l=` tag. The tag is a semicolon-
// separated tag-value entry per RFC 6376 §3.5; whitespace and folding
// can occur freely, so we reassemble each multi-line header before
// parsing.
//
// We err on the side of refusal: any `l=` (whether the value matches
// the actual body length or not) is treated as suspicious because the
// attacker model is "extend the body past the signed length" — even
// `l=<actual_length>` becomes exploitable the moment a downstream
// MTA or replay re-frames the message.
func dkimSignatureHasBodyLengthTag(rawMessage []byte) bool {
	for _, tags := range dkimSignatureTags(rawMessage) {
		if _, ok := tags["l"]; ok {
			return true
		}
	}
	return false
}

// dkimSignatureTags returns tag maps in header order so they can be correlated
// with go-msgauth's ordered verification results. Header folding is unfolded
// before parsing.
func dkimSignatureTags(rawMessage []byte) []map[string]string {
	// Read the header block: lines up to (but not including) the first
	// empty line. RFC 5322 line ending is CRLF but we tolerate LF.
	scanner := bufio.NewScanner(bytes.NewReader(rawMessage))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var signatures []map[string]string
	var currentHeader strings.Builder
	inDKIM := false

	flush := func() {
		if !inDKIM {
			return
		}
		signatures = append(signatures, parseTagValues(currentHeader.String()))
		currentHeader.Reset()
		inDKIM = false
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			return signatures
		}
		// Folded continuation: starts with SP or HTAB.
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			if inDKIM {
				currentHeader.WriteByte(' ')
				currentHeader.WriteString(strings.TrimLeft(line, " \t"))
			}
			continue
		}
		// New header line — finalize the previous one first.
		flush()
		// Case-insensitive prefix match per RFC 5322.
		if colon := strings.IndexByte(line, ':'); colon > 0 {
			name := strings.TrimSpace(line[:colon])
			if strings.EqualFold(name, "DKIM-Signature") {
				inDKIM = true
				currentHeader.WriteString(strings.TrimLeft(line[colon+1:], " \t"))
			}
		}
	}
	if scanner.Err() != nil {
		return nil
	}
	flush()
	return signatures
}

// tagValueContainsKey reports whether a DKIM tag-value string (the
// part after `DKIM-Signature:`) carries the given key. RFC 6376 §3.2
// format: tag = name "=" value, entries separated by `;`, with
// optional FWS around the `=` and trimming on both sides. We don't
// care about the value here, only the presence of the key.
func tagValueContainsKey(s, key string) bool {
	_, ok := parseTagValues(s)[key]
	return ok
}

func parseTagValues(s string) map[string]string {
	values := make(map[string]string)
	for _, entry := range strings.Split(s, ";") {
		eq := strings.IndexByte(entry, '=')
		if eq < 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(entry[:eq]))
		if name != "" {
			values[name] = strings.TrimSpace(entry[eq+1:])
		}
	}
	return values
}

func extractDomain(email string) string {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return ""
}
