package filterquery

// Node is one AST node. Pos is the 0-based byte offset of the node's first
// character in the input, for error messages.
type Node interface {
	Pos() int
}

// And is a conjunction — explicit (expression level) or implicit (sequence
// level). The two levels share a node type; precedence is settled at parse.
type And struct {
	Terms []Node
	At    int
}

func (n *And) Pos() int { return n.At }

type Or struct {
	Terms []Node
	At    int
}

func (n *Or) Pos() int { return n.At }

type Not struct {
	X  Node
	At int
}

func (n *Not) Pos() int { return n.At }

// Comparison is `field op value`. Value holds the coerced value and is set
// by Registry.Validate; before validation only Raw is meaningful. A
// comparison may emit only through the registry that validated it.
type Comparison struct {
	Field       string // dotted path, e.g. "label" or "metrics.latency"
	Op          string // one of ":", "=", "!=", "<", "<=", ">", ">="
	Raw         string // value text (unescaped contents for quoted strings)
	Quoted      bool   // value was a quoted string
	Value       any    // coerced value, set by Validate
	validatedBy *Registry
	At          int
}

func (n *Comparison) Pos() int { return n.At }

// Bare is an unqualified term (AIP-160 global restriction). The v1 registry
// rejects these at validation; the parser still produces them so the
// rejection error can name the term precisely.
type Bare struct {
	Text string
	At   int
}

func (n *Bare) Pos() int { return n.At }
