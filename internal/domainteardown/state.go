// Package domainteardown defines the durable state shared by domain deletion,
// sender-identity convergence, and the public deletion receipt.
package domainteardown

// State is an open-set value at the HTTP boundary. The server currently
// persists these three values; clients must treat unknown future values as
// not confirmed.
type State string

// Receipt identifies one deleted domain registration. Incarnation is the
// deleted row's verification token: immutable for that registration and
// replaced on re-registration, so keyed retries can safely follow the old
// teardown without acting on a same-name replacement.
type Receipt struct {
	Incarnation string
	State       State
}

const (
	Confirmed    State = "confirmed"
	Pending      State = "pending"
	ManualReview State = "manual_review"
)
