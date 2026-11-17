package review

import (
	"fmt"
	"sort"
	"time"
)

// ConstraintSet accumulates constraints and answers feasibility queries.
type ConstraintSet struct {
	constraints []Constraint
	byID        map[string]int
	budget      *Budget
}

// NewConstraintSet builds an empty system with a search budget.
func NewConstraintSet(limit int) *ConstraintSet {
	if limit <= 0 {
		limit = 4000
	}
	return &ConstraintSet{byID: map[string]int{}, budget: &Budget{Limit: limit}}
}

// Budget exposes the running budget.
func (cs *ConstraintSet) Budget() Budget { return *cs.budget }

// Add appends a constraint. Duplicate ids are ignored.
func (cs *ConstraintSet) Add(c Constraint) {
	if _, ok := cs.byID[c.ID]; ok {
		return
	}
	if c.ID == "" {
		c.ID = fmt.Sprintf("c%d", len(cs.constraints)+1)
	}
	cs.byID[c.ID] = len(cs.constraints)
	cs.constraints = append(cs.constraints, c)
}

// Constraints returns the accumulated constraints.
func (cs *ConstraintSet) Constraints() []Constraint {
	return append([]Constraint(nil), cs.constraints...)
}

// Window pins a variable to an interval (relative to the epoch zero node).
func Window(cs *ConstraintSet, varName string, b TimeBound, records []string, reason string, idPrefix string) {
	earliest := b.Earliest.UnixNano()
	latest := b.Latest.UnixNano()
	cs.Add(Constraint{ID: idPrefix + "#hi", Kind: "window", From: varName, To: zeroNode,
		MaxGapNs: latest, Reason: reason + ": earliest <= t <= latest (upper bound)", RecordIDs: records})
	cs.Add(Constraint{ID: idPrefix + "#lo", Kind: "window", From: zeroNode, To: varName,
		MaxGapNs: -earliest, Reason: reason + ": earliest <= t <= latest (lower bound)", RecordIDs: records})
}

// Before asserts t[a] <= t[b].
func Before(cs *ConstraintSet, a, b string, records []string, reason, id string) {
	cs.Add(Constraint{ID: id, Kind: causal, From: a, To: b, MaxGapNs: 0,
		Reason: reason, RecordIDs: records})
}

// StrictBefore asserts t[a] < t[b] with one nanosecond of separation, which is
// the smallest gap the ledger's RFC3339Nano timestamps can express.
func StrictBefore(cs *ConstraintSet, a, b string, records []string, reason, id string) {
	cs.Add(Constraint{ID: id, Kind: "distinct", From: a, To: b, MaxGapNs: -1,
		Reason: reason, RecordIDs: records})
}

const causal = "causal"

// edge is a compiled constraint.
type edge struct {
	from, to string
	weight   int64
	c        *Constraint
}

// SolveResult is the outcome of one feasibility query.
type SolveResult struct {
	Feasible bool
	Times    map[string]int64 // variable -> unix ns, when feasible
	Conflict *ConflictSet
	Skipped  bool // budget exhausted before this query could run
}

func (cs *ConstraintSet) edges(extra []Constraint) []edge {
	out := make([]edge, 0, len(cs.constraints)+len(extra))
	for i := range cs.constraints {
		c := &cs.constraints[i]
		out = append(out, edge{c.From, c.To, c.MaxGapNs, c})
	}
	for i := range extra {
		c := &extra[i]
		out = append(out, edge{c.From, c.To, c.MaxGapNs, c})
	}
	return out
}

// Solve runs Bellman-Ford over the accumulated constraints plus an optional
// branch. Each call consumes one unit of the search budget.
func (cs *ConstraintSet) Solve(extra []Constraint) SolveResult {
	if cs.budget.Used >= cs.budget.Limit {
		cs.budget.Exhausted = true
		cs.budget.SkippedChecks++
		return SolveResult{Skipped: true}
	}
	cs.budget.Used++
	edges := cs.edges(extra)
	nodes := map[string]bool{zeroNode: true}
	for _, e := range edges {
		nodes[e.from] = true
		nodes[e.to] = true
	}
	dist := map[string]int64{}
	prev := map[string]edge{}
	for n := range nodes {
		dist[n] = 0
	}
	relaxedAt := map[string]bool{}
	var lastRelaxed string
	// |V| rounds; a relaxation in the final round proves a negative cycle.
	for i := 0; i < len(nodes); i++ {
		changed := false
		for _, e := range edges {
			if dist[e.from] > dist[e.to]+e.weight {
				dist[e.from] = dist[e.to] + e.weight
				prev[e.from] = e
				changed = true
				relaxedAt[e.from] = true
				lastRelaxed = e.from
			}
		}
		if !changed {
			return SolveResult{Feasible: true, Times: dist}
		}
	}
	// Still relaxing after |V| rounds: extract the cycle that keeps shrinking.
	conflict := cs.extractCycle(prev, lastRelaxed, len(nodes))
	return SolveResult{Feasible: false, Conflict: conflict}
}

// extractCycle walks predecessors back into the negative cycle and reports the
// originating records and conditions. The set is not claimed to be minimal.
func (cs *ConstraintSet) extractCycle(prev map[string]edge, start string, n int) *ConflictSet {
	if start == "" {
		return &ConflictSet{Message: "constraint system is unsatisfiable"}
	}
	cur := start
	for i := 0; i < n+1; i++ {
		e, ok := prev[cur]
		if !ok {
			break
		}
		cur = e.to
	}
	// cur is now inside the cycle; walk until it repeats.
	cycleStart := cur
	recSeen := map[string]bool{}
	var records []string
	var conds []string
	for i := 0; i < n+2; i++ {
		e, ok := prev[cur]
		if !ok {
			break
		}
		for _, r := range e.c.RecordIDs {
			if r != "" && !recSeen[r] {
				recSeen[r] = true
				records = append(records, r)
			}
		}
		conds = appendCond(conds, fmt.Sprintf("%s: %s", e.c.ID, e.c.Reason))
		cur = e.to
		if cur == cycleStart {
			break
		}
	}
	sort.Strings(records)
	sort.Strings(conds)
	return &ConflictSet{
		Records: records, Conditions: conds,
		Message: "no common order satisfies these constraints; the set is sufficient to prove a conflict " +
			"but is not claimed to be minimal or to name a single root cause",
	}
}

func appendCond(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

// Verify checks an assignment against the constraints (used by tests to confirm
// a solver answer independently). Variables not present in the assignment are
// zero; nodes are addressed exactly as the solver addresses them, so the zero
// node is looked up rather than special-cased (a value of 0 is the epoch under
// that convention, which is what the solver uses).
func (cs *ConstraintSet) Verify(times map[string]int64, extra []Constraint) error {
	for _, e := range cs.edges(extra) {
		from := times[e.from]
		to := times[e.to]
		if from > to+e.weight {
			return fmt.Errorf("violated %s: %s - %s = %s > %s", e.c.ID, e.from, e.to,
				time.Duration(from-to), time.Duration(e.weight))
		}
	}
	return nil
}
