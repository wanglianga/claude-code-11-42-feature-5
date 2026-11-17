package api_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestReviewSampleFilesAreImportable loads the shipped fictional logs and runs
// them through the API, asserting each sample computes the conclusion its README
// documents. This is what makes the sample files a real, verifiable deliverable
// rather than illustrative JSON.
func TestReviewSampleFilesAreImportable(t *testing.T) {
	srv, _ := newServer(t)
	base := srv.URL
	root := filepath.Join("..", "..", "samples", "review")

	load := func(name string, v any) {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
	}
	type triple struct{ scope, logs, run string }
	cases := []struct {
		name   string
		files  triple
		verify func(t *testing.T, run map[string]any)
	}{
		{
			name:  "01-clock-skew",
			files: triple{"01-clock-skew-scope.json", "01-clock-skew-logs.json", "01-clock-skew-run.json"},
			verify: func(t *testing.T, run map[string]any) {
				an := analysisOf(t, run)
				if verdictOf(an, "claim:0:shipment:AWB70000000001") != "both_explain" {
					t.Fatalf("clock-skew shipment claim should be both_explain: %v", assertionIDsOf(an))
				}
			},
		},
		{
			name:  "02-combined-claims",
			files: triple{"02-combined-claims-scope.json", "02-combined-claims-logs.json", "02-combined-claims-run.json"},
			verify: func(t *testing.T, run map[string]any) {
				an := analysisOf(t, run)
				if verdictOf(an, "combination") != "contradicted" {
					t.Fatalf("combination should be contradicted: %v", assertionIDsOf(an))
				}
			},
		},
		{
			name:  "03-read-before-publish",
			files: triple{"03-read-before-publish-scope.json", "03-read-before-publish-logs.json", "03-read-before-publish-run.json"},
			verify: func(t *testing.T, run map[string]any) {
				an := analysisOf(t, run)
				if verdictOf(an, "read:early.read") != "contradicted" {
					t.Fatalf("read citing unpublished version should be contradicted: %v", assertionIDsOf(an))
				}
			},
		},
		{
			name:  "04-restart",
			files: triple{"04-restart-scope.json", "04-restart-logs.json", "04-restart-run.json"},
			verify: func(t *testing.T, run map[string]any) {
				an := analysisOf(t, run)
				for _, c := range constraintsOf(an) {
					if c["kind"] != "distinct" {
						continue
					}
					ids, _ := c["record_ids"].([]any)
					for _, r := range ids {
						if r == "restart.boot1-n5" {
							t.Fatalf("sequence edge crosses a boot segment: %v", c)
						}
					}
				}
			},
		},
		{
			name:  "05-late-log",
			files: triple{"05-late-log-scope.json", "05-late-log-logs.json", "05-late-log-run.json"},
			verify: func(t *testing.T, run map[string]any) {
				an := analysisOf(t, run)
				if verdictOf(an, "read:late.read") != "" {
					t.Fatalf("late read must not appear in the old case: %v", assertionIDsOf(an))
				}
			},
		},
		{
			name:  "06-shared-prereq",
			files: triple{"06-shared-prereq-scope.json", "06-shared-prereq-logs.json", "06-shared-prereq-run.json"},
			verify: func(t *testing.T, run map[string]any) {
				an := analysisOf(t, run)
				unresolved, _ := an["unresolved"].([]any)
				found := false
				for _, u := range unresolved {
					s, _ := u.(string)
					if containsSub(s, "shared.hub-feed") {
						found = true
					}
				}
				if !found {
					t.Fatalf("out-of-scope dependency must remain unresolved: %v", unresolved)
				}
			},
		},
		{
			name:  "08-multi-shipment",
			files: triple{"08-multi-shipment/scope.json", "08-multi-shipment/logs.json", "08-multi-shipment/run.json"},
			verify: func(t *testing.T, run map[string]any) {
				an := analysisOf(t, run)
				if verdictOf(an, "combination") != "consistent" {
					t.Fatalf("two verified shipments should share one order: %v", assertionIDsOf(an))
				}
			},
		},
		{
			name:  "09-transload",
			files: triple{"09-transload/scope.json", "09-transload/logs.json", "09-transload/run.json"},
			verify: func(t *testing.T, run map[string]any) {
				an := analysisOf(t, run)
				if verdictOf(an, "claim:0:container:AKE-TL-B") != "contradicted" {
					t.Fatalf("later container at an earlier instant must be contradicted: %v", assertionIDsOf(an))
				}
			},
		},
		{
			name:  "10-snapshot-stitching",
			files: triple{"10-snapshot-stitching/scope.json", "10-snapshot-stitching/logs.json", "10-snapshot-stitching/run.json"},
			verify: func(t *testing.T, run map[string]any) {
				an := analysisOf(t, run)
				if verdictOf(an, "claim:0:shipment:AWB70000000006C") != "contradicted" {
					t.Fatalf("stitched membership/count must be rejected: %v", assertionIDsOf(an))
				}
			},
		},
		{
			name:  "11-image-dependent",
			files: triple{"11-image-dependent/scope.json", "11-image-dependent/logs.json", "11-image-dependent/run.json"},
			verify: func(t *testing.T, run map[string]any) {
				an := analysisOf(t, run)
				if verdictOf(an, "claim:0:shipment:AWB70000000006D") != "undetermined" {
					t.Fatalf("image-only content must stay undetermined: %v", assertionIDsOf(an))
				}
				if len(constraintsOf(an)) == 0 {
					t.Fatal("image gap must not remove existing time relations")
				}
			},
		},
		{
			name:  "07-budget",
			files: triple{"07-budget/scope.json", "07-budget/logs.json", "07-budget/run.json"},
			verify: func(t *testing.T, run map[string]any) {
				an := analysisOf(t, run)
				summary, _ := an["summary"].(map[string]any)
				if ex, _ := summary["budget_exhausted"].(bool); !ex {
					t.Fatalf("budget must be exhausted: %v", summary)
				}
				if v, _ := summary["verdict"].(string); v == "contradicted" {
					t.Fatalf("budget stop must not read as contradiction: %v", summary)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var scope, logs, run map[string]any
			load(c.files.scope, &scope)
			load(c.files.logs, &logs)
			load(c.files.run, &run)
			scopeOut := post(t, base, "/v1/reviews/scopes", scope, 201)
			scopeID := scopeOut["scope_id"].(string)
			caseOut := post(t, base, "/v1/reviews/cases", map[string]any{
				"scope_id": scopeID, "title": c.name,
			}, 201)
			caseID := caseOut["case_id"].(string)
			post(t, base, "/v1/reviews/scopes/"+scopeID+"/imports", logs, 201)
			runOut := post(t, base, "/v1/reviews/cases/"+caseID+"/runs", run, 201)
			c.verify(t, runOut)
		})
	}
}

func analysisOf(t *testing.T, run map[string]any) map[string]any {
	t.Helper()
	r, _ := run["run"].(map[string]any)
	an, _ := r["analysis"].(map[string]any)
	if an == nil {
		t.Fatalf("run has no analysis: %v", run)
	}
	return an
}

func verdictOf(an map[string]any, id string) string {
	arr, _ := an["assertions"].([]any)
	for _, a := range arr {
		m, _ := a.(map[string]any)
		if m["assertion_id"] == id {
			v, _ := m["verdict"].(string)
			return v
		}
	}
	return ""
}

func assertionIDsOf(an map[string]any) []string {
	var out []string
	arr, _ := an["assertions"].([]any)
	for _, a := range arr {
		m, _ := a.(map[string]any)
		id, _ := m["assertion_id"].(string)
		v, _ := m["verdict"].(string)
		out = append(out, id+"="+v)
	}
	return out
}

func constraintsOf(an map[string]any) []map[string]any {
	var out []map[string]any
	arr, _ := an["constraints"].([]any)
	for _, c := range arr {
		m, _ := c.(map[string]any)
		out = append(out, m)
	}
	return out
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
