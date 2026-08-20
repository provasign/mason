package agent

// Obligations: goal-state verification for completion claims.
//
// The general failure this closes (measured live, 2026-08-19, gin rename on
// a local 30B): when a change breaks the build, the model optimizes for the
// visible signal — green — rather than the task's goal, and the harness
// could not tell the difference. The model renamed correctly, broke the
// build applying ambiguous edits, "recovered" by applying the exact inverse
// plan, and summarized success over a net-zero tree. Undoing the task is
// one instance; deleting a failing test or weakening an assertion are the
// same class: effort-state green, goal-state failed.
//
// Mechanism: every structured operation the harness applies registers an
// engine-checkable postcondition. At completion-claim time the open
// obligations are checked against the INDEX (type-aware, no string
// fragility); an unmet one rejects the claim once with its specific text —
// the same reject-once-then-flag contract as the untouched-tree guard.
// Nothing is ever refused mid-task: an inverse rename plan stays fully
// available (it may be right), it just cannot be summarized as success
// while the obligation stands.
//
// Checks FAIL OPEN on engine errors: a broken index must not convert into
// a false accusation of fabrication.

import (
	"fmt"
	"strings"
)

// obligation is one engine-checkable postcondition of the current task.
type obligation struct {
	desc  string
	check func(s *Session) (met bool, detail string)
}

// registerRenameObligation converts an applied rename plan into the
// postcondition "new name resolves; old name does not".
func (s *Session) registerRenameObligation(plan map[string]any) {
	oldQ, _ := plan["query"].(string)
	newLeaf, _ := plan["newName"].(string)
	if oldQ == "" || newLeaf == "" {
		return
	}
	newQ := oldQ
	if i := strings.LastIndex(oldQ, "."); i >= 0 {
		newQ = oldQ[:i+1] + newLeaf
	} else {
		newQ = newLeaf
	}
	// A later inverse plan registers the mirrored obligation; keep only the
	// most recent rename obligation for the same symbol pair so the ledger
	// reflects the LAST applied plan, and the completion check tells the
	// truth about the tree as it stands. The task-level goal is judged by
	// whichever obligation remains unmet against the task's own words —
	// which is exactly what the model must argue if it believes the final
	// state is right.
	kept := s.obligations[:0]
	for _, o := range s.obligations {
		if !strings.HasPrefix(o.desc, "rename: ") {
			kept = append(kept, o)
		}
	}
	s.obligations = kept

	desc := fmt.Sprintf("rename: %s must resolve in the index and %s must not (the applied plan renamed %s -> %s)",
		newQ, oldQ, oldQ, newLeaf)
	s.obligations = append(s.obligations, obligation{
		desc: desc,
		check: func(s *Session) (bool, string) {
			newOK, err1 := s.symbolResolves(newQ)
			oldGone, err2 := s.symbolResolves(oldQ)
			if err1 != nil || err2 != nil {
				return true, "" // fail open: engine unavailable is not fabrication
			}
			switch {
			case newOK && !oldGone:
				return true, ""
			case !newOK && oldGone:
				return false, fmt.Sprintf("%s does not resolve but %s still does — the rename is not in effect", newQ, oldQ)
			case !newOK:
				return false, fmt.Sprintf("neither %s nor %s resolves — the tree lost the symbol", newQ, oldQ)
			default:
				return false, fmt.Sprintf("both %s and %s resolve — the rename is incomplete", newQ, oldQ)
			}
		},
	})
}

// symbolResolves asks the index whether a (possibly Type.qualified) symbol
// currently exists. Uses prism_lookup's exact-match contract: a miss carries
// matched=false; an exact hit carries the symbol with no matched field.
func (s *Session) symbolResolves(name string) (bool, error) {
	if s.invoke == nil {
		return false, fmt.Errorf("no engine")
	}
	res, err := s.invoke("prism_lookup", map[string]any{"name": name})
	if err != nil {
		// "no symbol named X" style errors are a definitive NO, not an
		// engine failure — but lookup returns those as results, not errors,
		// so any error here is infrastructure.
		return false, err
	}
	m, ok := res.(map[string]any)
	if !ok {
		return false, fmt.Errorf("unexpected lookup shape")
	}
	if matched, present := m["matched"].(bool); present && !matched {
		return false, nil
	}
	return m["symbol"] != nil, nil
}

// checkObligations delta-indexes once, then evaluates every open obligation,
// returning the specific text of each unmet one.
func (s *Session) checkObligations() []string {
	if len(s.obligations) == 0 || s.invoke == nil {
		return nil
	}
	_, _ = s.invoke("prism_index", map[string]any{}) // check against the tree as it stands
	var unmet []string
	for _, o := range s.obligations {
		met, detail := o.check(s)
		if met {
			continue
		}
		msg := o.desc
		if detail != "" {
			msg = detail
		}
		unmet = append(unmet, msg)
	}
	return unmet
}
