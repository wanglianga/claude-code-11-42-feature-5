package review

import (
	"fmt"
	"sort"
	"time"
)

// CrossCheck compares the targeted solver with a small-scale exhaustive
// enumeration. It is a verification tool (used by tests and the demo) that
// confirms targeted answers on a material small enough to enumerate completely.
// It intentionally does not export every permutation from production paths.
type CrossCheck struct {
	CaseID            string   `json:"case_id"`
	AssertionID       string   `json:"assertion_id"`
	TargetedVerdict   string   `json:"targeted_verdict"`
	ExhaustiveVerdict string   `json:"exhaustive_verdict"`
	Agree             bool     `json:"agreement"`
	Permutations      int      `json:"permutations_checked"`
	CappedAt          int      `json:"cap,omitempty"`
	Notes             []string `json:"notes,omitempty"`
}

// VerifyAgainstExhaustive enumerates the occurrence orders of a small record set
// and reproduces a status assertion's verdict independently. It does not use the
// difference-constraint encoding: it places the relevant publication versions in
// every order consistent with their own platform timestamps and checks which
// version a query at the instant would have resolved to. When the version count
// exceeds the cap it reports the cap and refuses to claim agreement.
func VerifyAgainstExhaustive(w *World, results []AssertionResult, cap int) []CrossCheck {
	if cap <= 0 {
		cap = 8
	}
	var out []CrossCheck
	assertions := w.BuildAssertions()
	for _, a := range assertions {
		if a.Kind != AssertionStatus && a.Kind != AssertionClaim && a.Kind != AssertionConsumable {
			continue
		}
		at, ok := statusInstant(a, w.Target)
		if !ok || a.Subject == nil {
			continue
		}
		versions := w.Versions(a.Subject.Type, a.Subject.Key)
		if len(versions) == 0 {
			continue
		}
		var targeted string
		for _, r := range results {
			if r.AssertionID == a.ID {
				targeted = r.Verdict
			}
		}
		perm, pos, neg, capped := enumerateStatus(at, versions, a, cap)
		verdict := VerdictUndetermined
		switch {
		case pos && neg:
			verdict = VerdictBothExplain
		case pos && !neg:
			verdict = VerdictConsistent
		case !pos && neg:
			verdict = VerdictContradicted
		}
		cc := CrossCheck{
			CaseID: w.Target.CaseID, AssertionID: a.ID,
			TargetedVerdict: targeted, ExhaustiveVerdict: verdict,
			Agree: targeted == verdict, Permutations: perm,
		}
		if capped {
			cc.CappedAt = cap
			cc.Agree = false
			cc.Notes = append(cc.Notes,
				fmt.Sprintf("exhaustive check capped at %d permutations without finishing; no agreement claimed", cap))
		}
		if !cc.Agree && !capped {
			cc.Notes = append(cc.Notes,
				"targeted and exhaustive checks disagree; inspect the material before trusting either")
		}
		out = append(out, cc)
	}
	return out
}

// enumerateStatus is the small-scale brute force. The instant is a free variable
// inside the claim's interval; for every version-order permutation it samples
// the interval endpoints and every publication timestamp inside the interval
// (status can only change at a publication boundary). It reports whether some
// candidate instant reads the claimed status (positive) and whether some reads
// something else (negative). The instant is treated as exact for each sample,
// which is a strictly finer partition than the interval reading and is why the
// two methods can be compared honestly.
func enumerateStatus(at TimeBound, versions []*Record, a Assertion, cap int) (int, bool, bool, bool) {
	n := len(versions)
	if factorial(n) > cap {
		return 0, false, false, true
	}
	cands := []time.Time{at.Earliest, at.Latest}
	for _, v := range versions {
		t := v.PublishedAt.UTC()
		if t.After(at.Earliest) && t.Before(at.Latest) {
			cands = append(cands, t, t.Add(time.Nanosecond))
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Before(cands[j]) })
	count := 0
	pos, neg := false, false
	for _, perm := range permutations(n) {
		count++
		ok := true
		for i := 0; i+1 < len(perm); i++ {
			if versions[perm[i]].PublishedAt.After(versions[perm[i+1]].PublishedAt) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		for _, t := range cands {
			if t.Before(at.Earliest) || t.After(at.Latest) {
				continue
			}
			var eff *Record
			for _, i := range perm {
				v := versions[i]
				if !v.PublishedAt.After(t) {
					eff = v
				}
			}
			if eff == nil {
				neg = true
				continue
			}
			if statusMatches(eff, a) {
				pos = true
			} else {
				neg = true
			}
		}
	}
	return count, pos, neg, false
}

func factorial(n int) int {
	if n <= 1 {
		return 1
	}
	out := 1
	for i := 2; i <= n; i++ {
		out *= i
	}
	return out
}

func permutations(n int) [][]int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	var out [][]int
	var rec func(k int)
	rec = func(k int) {
		if k == n {
			out = append(out, append([]int(nil), idx...))
			return
		}
		for i := k; i < n; i++ {
			idx[k], idx[i] = idx[i], idx[k]
			rec(k + 1)
			idx[k], idx[i] = idx[i], idx[k]
		}
	}
	rec(0)
	return out
}

// SortExamples returns examples in a stable, reviewable order.
func SortExamples(ex []OrderExample) []OrderExample {
	out := append([]OrderExample(nil), ex...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Note < out[j].Note
	})
	return out
}
