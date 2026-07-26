package filterquery

import "fmt"

// traversalState protects public/manual AST traversal with the parser's
// depth and node limits, and rejects only identity cycles on the active path.
// A completed subtree is removed from active so a shared subtree remains
// valid and counts once for every occurrence.
type traversalState struct {
	nodes  int
	depth  int
	active map[any]struct{}
}

func newTraversalState() *traversalState {
	return &traversalState{active: make(map[any]struct{})}
}

// enter records n and returns a closure that must run after visiting its
// children. It validates pointer shapes before accessing their fields.
func (s *traversalState) enter(n Node) (func(), error) {
	pos, identity, err := traversalNodeInfo(n)
	if err != nil {
		return nil, err
	}
	if s.depth > maxDepth {
		return nil, &Error{Kind: ErrCap, Pos: pos, Msg: fmt.Sprintf("expression nested too deeply (limit %d)", maxDepth)}
	}
	s.nodes++
	if s.nodes > maxNodes {
		return nil, &Error{Kind: ErrCap, Pos: pos, Msg: fmt.Sprintf("expression too large (limit %d nodes)", maxNodes)}
	}
	if _, ok := s.active[identity]; ok {
		return nil, &Error{Kind: ErrValidate, Pos: pos, Msg: "filterquery: cyclic AST is not supported"}
	}
	s.active[identity] = struct{}{}
	s.depth++
	return func() {
		s.depth--
		delete(s.active, identity)
	}, nil
}

func traversalNodeInfo(n Node) (int, any, error) {
	switch t := n.(type) {
	case nil:
		return -1, nil, &Error{Kind: ErrValidate, Pos: -1, Msg: "filterquery: cannot traverse nil node"}
	case *And:
		if t == nil {
			return -1, nil, &Error{Kind: ErrValidate, Pos: -1, Msg: "filterquery: cannot traverse nil *And"}
		}
		return t.At, t, nil
	case *Or:
		if t == nil {
			return -1, nil, &Error{Kind: ErrValidate, Pos: -1, Msg: "filterquery: cannot traverse nil *Or"}
		}
		return t.At, t, nil
	case *Not:
		if t == nil {
			return -1, nil, &Error{Kind: ErrValidate, Pos: -1, Msg: "filterquery: cannot traverse nil *Not"}
		}
		return t.At, t, nil
	case *Bare:
		if t == nil {
			return -1, nil, &Error{Kind: ErrValidate, Pos: -1, Msg: "filterquery: cannot traverse nil *Bare"}
		}
		return t.At, t, nil
	case *Comparison:
		if t == nil {
			return -1, nil, &Error{Kind: ErrValidate, Pos: -1, Msg: "filterquery: cannot traverse nil *Comparison"}
		}
		return t.At, t, nil
	default:
		return -1, nil, &Error{Kind: ErrValidate, Pos: -1, Msg: fmt.Sprintf("filterquery: unknown node type %T", n)}
	}
}
