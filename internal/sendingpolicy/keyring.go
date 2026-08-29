package sendingpolicy

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Secret payload bounds. These mirror the limits the ops-side versioned secret
// resolver already enforces before the value ever reaches this process, so a
// payload that passed there cannot surprise the parser here.
const (
	maxSecretPayloadBytes = 65536
	minKeyBytes           = 32
	maxKeyBytes           = 4096
	maxSecretItems        = 128
	maxMailboxLength      = 254
	maxLocalPartLength    = 64
	maxDomainLabelLength  = 63
)

// Commitment labels are fixed strings and part of the cross-slot contract: two
// slots must derive identical commitments from identical secrets, so these can
// never be environment-dependent.
const (
	operatorCommitmentLabel = "e2a-operator-notice-v1"
	operatorKeyIDLabel      = "e2a-operator-notice-key-id-v1"
)

// errRedacted is the shape every failure in this file takes. A malformed secret
// must never be echoed: these values are HMAC keys and operator mailboxes, and
// a startup log is the last place either should appear. Callers get the reason
// and the offending version, never the payload.
func errRedacted(format string, args ...any) error {
	return fmt.Errorf("sendingpolicy: "+format, args...)
}

// Keyring is the immutable HMAC keyring loaded from
// E2A_SENDING_FEEDBACK_HMAC_KEYS. One version signs new material; every version
// stays available to verify feedback that was signed before a rotation.
type Keyring struct {
	active int
	keys   map[int][]byte
}

// keyringPayload is the exact wire form. Unknown fields are rejected by the
// DisallowUnknownFields decoder rather than ignored, matching the ops-side
// validator that refuses any payload whose key set is not exactly this.
type keyringPayload struct {
	Active *int              `json:"active"`
	Keys   map[string]string `json:"keys"`
}

// LoadKeyring parses and validates the keyring secret. Every failure is fatal
// at startup: a deployment that cannot sign feedback correlations must not come
// up believing it can.
func LoadKeyring(raw string) (*Keyring, error) {
	if err := checkSecretEnvelope(raw, "hmac keyring"); err != nil {
		return nil, err
	}

	var payload keyringPayload
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, errRedacted("hmac keyring is not valid JSON")
	}
	if dec.More() {
		return nil, errRedacted("hmac keyring has trailing content")
	}
	if payload.Active == nil {
		return nil, errRedacted("hmac keyring is missing the active version")
	}
	if len(payload.Keys) == 0 {
		return nil, errRedacted("hmac keyring contains no keys")
	}
	if len(payload.Keys) > maxSecretItems {
		return nil, errRedacted("hmac keyring carries %d keys, limit is %d", len(payload.Keys), maxSecretItems)
	}

	keys := make(map[int][]byte, len(payload.Keys))
	for rawVersion, encoded := range payload.Keys {
		version, err := parseSecretVersion(rawVersion)
		if err != nil {
			return nil, errRedacted("hmac keyring has an invalid key version")
		}
		key, err := decodeSecretKey(encoded)
		if err != nil {
			return nil, errRedacted("hmac keyring key version %d is invalid: %v", version, err)
		}
		keys[version] = key
	}

	// The active version must be present. Signing with a version the keyring
	// cannot resolve would produce correlations nothing can ever verify.
	if _, ok := keys[*payload.Active]; !ok {
		return nil, errRedacted("hmac keyring active version %d has no key", *payload.Active)
	}
	return &Keyring{active: *payload.Active, keys: keys}, nil
}

// ActiveVersion reports which key new material is signed under.
func (k *Keyring) ActiveVersion() int { return k.active }

// Versions lists every loaded version in ascending order. Used by the deploy
// gate to prove one slot's keyring is a superset of the other's.
func (k *Keyring) Versions() []int {
	out := make([]int, 0, len(k.keys))
	for v := range k.keys {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

// Sign returns the HMAC-SHA256 of msg under the active key, with the version it
// used so a verifier can select the same key after a rotation.
func (k *Keyring) Sign(msg []byte) (version int, mac []byte) {
	return k.active, hmacSHA256(k.keys[k.active], msg)
}

// Verify checks msg against mac under a specific version. An unknown version is
// a failure, never a fallback to the active key: accepting material signed by a
// key this process does not hold would defeat the correlation entirely.
func (k *Keyring) Verify(version int, msg, mac []byte) bool {
	key, ok := k.keys[version]
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare(hmacSHA256(key, msg), mac) == 1
}

// OperatorRecipients is the immutable versioned operator mailbox map loaded
// from E2A_SENDING_PROTECTION_OPERATOR_EMAILS.
//
// The addresses are secret deployment configuration, not part of the account
// model, so this type deliberately exposes commitments freely and mailboxes
// narrowly: capabilities and registry rows carry only the HMAC commitment, and
// only the notice sender ever resolves an actual address.
type OperatorRecipients struct {
	keyID       string
	recipients  map[int]string
	commitments map[int]string
}

type operatorPayload struct {
	CommitmentKey string            `json:"commitment_key"`
	Recipients    map[string]string `json:"recipients"`
}

// LoadOperatorRecipients parses and validates the operator map, deriving the
// key identity and per-version commitments that the registry and the capability
// readback compare against.
func LoadOperatorRecipients(raw string) (*OperatorRecipients, error) {
	if err := checkSecretEnvelope(raw, "operator recipient map"); err != nil {
		return nil, err
	}

	var payload operatorPayload
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return nil, errRedacted("operator recipient map is not valid JSON")
	}
	if dec.More() {
		return nil, errRedacted("operator recipient map has trailing content")
	}
	if payload.Recipients == nil {
		return nil, errRedacted("operator recipient map is missing the recipients object")
	}
	if len(payload.Recipients) == 0 {
		return nil, errRedacted("operator recipient map contains no recipients")
	}
	if len(payload.Recipients) > maxSecretItems {
		return nil, errRedacted("operator recipient map carries %d versions, limit is %d",
			len(payload.Recipients), maxSecretItems)
	}

	key, err := decodeSecretKey(payload.CommitmentKey)
	if err != nil {
		return nil, errRedacted("operator recipient map commitment key is invalid: %v", err)
	}

	recipients := make(map[int]string, len(payload.Recipients))
	commitments := make(map[int]string, len(payload.Recipients))
	// Two versions resolving to the same mailbox would make a rotation look
	// complete when nothing actually changed, so normalized duplicates are a
	// configuration error rather than a harmless redundancy.
	seen := make(map[string]int, len(payload.Recipients))

	for rawVersion, rawMailbox := range payload.Recipients {
		version, err := parseSecretVersion(rawVersion)
		if err != nil {
			return nil, errRedacted("operator recipient map has an invalid version")
		}
		mailbox, err := normalizeMailbox(rawMailbox)
		if err != nil {
			return nil, errRedacted("operator recipient map version %d has an invalid mailbox: %v", version, err)
		}
		if prior, dup := seen[mailbox]; dup {
			return nil, errRedacted("operator recipient map versions %d and %d resolve to the same mailbox", prior, version)
		}
		seen[mailbox] = version
		recipients[version] = mailbox
		commitments[version] = operatorCommitment(key, version, mailbox)
	}

	return &OperatorRecipients{
		keyID:       hex.EncodeToString(hmacSHA256(key, []byte(operatorKeyIDLabel))),
		recipients:  recipients,
		commitments: commitments,
	}, nil
}

// KeyID is the lowercase HMAC-SHA256 of the commitment key over a fixed label.
// It identifies the key across slots and registry rows without revealing it.
func (o *OperatorRecipients) KeyID() string { return o.keyID }

// Commitment returns the lowercase HMAC-SHA256 binding a version to its
// mailbox. This is what the append-only registry stores and what the capability
// readback advertises.
func (o *OperatorRecipients) Commitment(version int) (string, bool) {
	c, ok := o.commitments[version]
	return c, ok
}

// Commitments returns a defensive copy of every version's commitment, keyed by
// canonical decimal version for the capability payload.
func (o *OperatorRecipients) Commitments() map[string]string {
	out := make(map[string]string, len(o.commitments))
	for version, commitment := range o.commitments {
		out[strconv.Itoa(version)] = commitment
	}
	return out
}

// Mailbox resolves the address for a version. Only the notice sender calls it;
// nothing else in the system needs the plaintext.
func (o *OperatorRecipients) Mailbox(version int) (string, bool) {
	m, ok := o.recipients[version]
	return m, ok
}

// Versions lists every configured version in ascending order.
func (o *OperatorRecipients) Versions() []int {
	out := make([]int, 0, len(o.recipients))
	for v := range o.recipients {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

// operatorCommitment binds label, canonical decimal version, and normalized
// mailbox with NUL separators. The separator matters: without it, version 12
// with mailbox "3@x" and version 123 with mailbox "@x" would commit to the same
// bytes, letting a rotation appear to have happened when it had not.
func operatorCommitment(key []byte, version int, mailbox string) string {
	var msg []byte
	msg = append(msg, operatorCommitmentLabel...)
	msg = append(msg, 0)
	msg = append(msg, strconv.Itoa(version)...)
	msg = append(msg, 0)
	msg = append(msg, mailbox...)
	return hex.EncodeToString(hmacSHA256(key, msg))
}

func hmacSHA256(key, msg []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(msg)
	return mac.Sum(nil)
}

// checkSecretEnvelope applies the byte-level rules before any JSON parsing, so
// a payload that would be unsafe to even quote in an error is rejected first.
func checkSecretEnvelope(raw, what string) error {
	if raw == "" {
		return errRedacted("%s is empty", what)
	}
	if len(raw) > maxSecretPayloadBytes {
		return errRedacted("%s is %d bytes, limit is %d", what, len(raw), maxSecretPayloadBytes)
	}
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c < 0x20 || c > 0x7e {
			return errRedacted("%s contains a non-printable byte", what)
		}
	}
	return nil
}

// parseSecretVersion accepts only canonical positive decimals of at most nine
// digits. Rejecting "0", "01", and "+1" keeps the string form a stable map key:
// two spellings of the same number would otherwise be two registry versions.
func parseSecretVersion(raw string) (int, error) {
	if len(raw) == 0 || len(raw) > 9 || raw[0] < '1' || raw[0] > '9' {
		return 0, errRedacted("version is not a canonical positive integer")
	}
	for i := 1; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, errRedacted("version is not a canonical positive integer")
		}
	}
	return strconv.Atoi(raw)
}

// decodeSecretKey requires unpadded base64url in its canonical encoding. A
// non-canonical spelling that decodes to the same bytes is rejected, because
// two slots comparing encoded strings must agree byte-for-byte.
func decodeSecretKey(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, errRedacted("key is empty")
	}
	for i := 0; i < len(encoded); i++ {
		c := encoded[i]
		isBase64URL := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_'
		if !isBase64URL {
			return nil, errRedacted("key is not unpadded base64url")
		}
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errRedacted("key is not decodable base64url")
	}
	if base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return nil, errRedacted("key encoding is not canonical")
	}
	if len(decoded) < minKeyBytes {
		return nil, errRedacted("key is shorter than %d bytes", minKeyBytes)
	}
	if len(decoded) > maxKeyBytes {
		return nil, errRedacted("key is longer than %d bytes", maxKeyBytes)
	}
	return decoded, nil
}

// normalizeMailbox lowercases and validates one plain addr-spec.
//
// This is deliberately much stricter than RFC 5321. The value is an operator
// address that ends up as an SMTP envelope recipient for pause notices, so
// display names, route syntax, quoted local parts, and non-ASCII are all
// rejected rather than interpreted — there is no legitimate operator inbox that
// needs them, and each one is a place an injected address could hide.
func normalizeMailbox(raw string) (string, error) {
	if raw == "" {
		return "", errRedacted("mailbox is empty")
	}
	if len(raw) > maxMailboxLength {
		return "", errRedacted("mailbox is longer than %d bytes", maxMailboxLength)
	}
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c < 0x21 || c > 0x7e {
			return "", errRedacted("mailbox contains a control, space, or non-ASCII byte")
		}
	}

	at := strings.IndexByte(raw, '@')
	if at < 0 || at != strings.LastIndexByte(raw, '@') {
		return "", errRedacted("mailbox must contain exactly one @")
	}
	local, domain := raw[:at], strings.ToLower(raw[at+1:])

	if err := validateLocalPart(local); err != nil {
		return "", err
	}
	if err := validateDomain(domain); err != nil {
		return "", err
	}
	return strings.ToLower(local) + "@" + domain, nil
}

func validateLocalPart(local string) error {
	if local == "" || len(local) > maxLocalPartLength {
		return errRedacted("mailbox local part is empty or longer than %d bytes", maxLocalPartLength)
	}
	if local[0] == '.' || local[len(local)-1] == '.' {
		return errRedacted("mailbox local part has a leading or trailing dot")
	}
	if strings.Contains(local, "..") {
		return errRedacted("mailbox local part has consecutive dots")
	}
	// RFC 5322 atext plus dot. Everything excluded here — angle brackets,
	// quotes, commas, semicolons, colons, backslashes, parentheses — is a
	// separator in some address grammar, and none belongs in a bare mailbox.
	const atext = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!#$%&'*+-/=?^_`{|}~."
	for i := 0; i < len(local); i++ {
		if !strings.ContainsRune(atext, rune(local[i])) {
			return errRedacted("mailbox local part contains a disallowed character")
		}
	}
	return nil
}

func validateDomain(domain string) error {
	if domain == "" {
		return errRedacted("mailbox domain is empty")
	}
	labels := strings.Split(domain, ".")
	// A single-label domain is never a routable public mailbox and is a common
	// shape for an internal-only or typo'd address.
	if len(labels) < 2 {
		return errRedacted("mailbox domain must have at least two labels")
	}
	for _, label := range labels {
		if label == "" || len(label) > maxDomainLabelLength {
			return errRedacted("mailbox domain label is empty or longer than %d bytes", maxDomainLabelLength)
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return errRedacted("mailbox domain label has a leading or trailing hyphen")
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-'
			if !ok {
				return errRedacted("mailbox domain label contains a disallowed character")
			}
		}
	}
	return nil
}
