package filterquery

import "strings"

// sexpr renders an AST as an s-expression for compact assertions.
func sexpr(n Node) string {
	switch t := n.(type) {
	case *And:
		parts := make([]string, len(t.Terms))
		for i, x := range t.Terms {
			parts[i] = sexpr(x)
		}
		return "(and " + strings.Join(parts, " ") + ")"
	case *Or:
		parts := make([]string, len(t.Terms))
		for i, x := range t.Terms {
			parts[i] = sexpr(x)
		}
		return "(or " + strings.Join(parts, " ") + ")"
	case *Not:
		return "(not " + sexpr(t.X) + ")"
	case *Comparison:
		return "(" + t.Field + " " + t.Op + " " + t.Raw + ")"
	case *Bare:
		return "(bare " + t.Text + ")"
	default:
		return "<?>"
	}
}
