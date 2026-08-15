// Package domainteardown defines the durable state shared by domain deletion,
// sender-identity convergence, and the public deletion receipt.
package domainteardown

// State is an open-set value at the HTTP boundary. The server currently
// persists these three values; clients must treat unknown future values as
// not confirmed.
type State string

const (
	Confirmed    State = "confirmed"
	Pending      State = "pending"
	ManualReview State = "manual_review"
)
