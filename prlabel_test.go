package main

import "testing"

func TestPRDisplay(t *testing.T) {
	cases := []struct {
		pr           sharedPR
		label, state string
	}{
		{sharedPR{Number: 1, State: "open"}, "pr#1", ""},
		{sharedPR{Number: 2, State: "draft"}, "pr#2 draft", "draft"},
		{sharedPR{Number: 3, State: "merged", Checks: "failure", Mergeable: "conflicting"}, "pr#3 merged", "merged"},
		{sharedPR{Number: 4, State: "closed", Review: "approved"}, "pr#4 closed", "closed"},
		{sharedPR{Number: 5, State: "open", Review: "approved", Checks: "success", Mergeable: "mergeable"}, "pr#5 approved", "approved"},
		{sharedPR{Number: 6, State: "open", Review: "changes_requested"}, "pr#6 changes requested", "changes requested"},
		{sharedPR{Number: 7, State: "open", Review: "review_required", Checks: "failure"}, "pr#7 review, ci failed", "review"},
		{sharedPR{Number: 8, State: "open", Checks: "pending"}, "pr#8 ci pending", ""},
		{sharedPR{Number: 9, State: "draft", Mergeable: "conflicting", Checks: "pending"}, "pr#9 draft, conflicts, ci pending", "draft"},
		{sharedPR{Number: 10, State: "open", Review: "approved", Mergeable: "conflicting", Checks: "failure"}, "pr#10 approved, conflicts, ci failed", "approved"},
	}
	for _, c := range cases {
		label, state := prDisplay(c.pr)
		if label != c.label || state != c.state {
			t.Errorf("prDisplay(%+v) = %q, %q; want %q, %q", c.pr, label, state, c.label, c.state)
		}
	}
}
