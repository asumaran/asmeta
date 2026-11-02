package main

// What the sidebar says of a PR, from the shared cache's normalized facts.

import (
	"strconv"
	"strings"
)

// prDisplay is the $pr label and the $pr_state token of pr.
//
// The label is "pr#N", then the state, then what stands in the way of an
// open PR, comma separated: "pr#12 draft", "pr#12 approved",
// "pr#12 review, ci failed", "pr#12 draft, conflicts, ci pending".
//
// The state is draft, merged or closed as before; an open PR's state is
// its review decision (approved, changes requested, or review while the
// repo's rule still waits for one). An open PR of a repo with no review
// rule has no state word: a bare "pr#N" reads as open. Conflicts and checks
// (ci failed, ci pending) are said only while the PR is open or draft.
func prDisplay(pr sharedPR) (label, state string) {
	state = pr.State
	if state == "open" {
		switch pr.Review {
		case "approved":
			state = "approved"
		case "changes_requested":
			state = "changes requested"
		case "review_required":
			state = "review"
		default:
			state = ""
		}
	}
	parts := []string{}
	if state != "" {
		parts = append(parts, state)
	}
	if pr.State == "open" || pr.State == "draft" {
		if pr.Mergeable == "conflicting" {
			parts = append(parts, "conflicts")
		}
		switch pr.Checks {
		case "failure":
			parts = append(parts, "ci failed")
		case "pending":
			parts = append(parts, "ci pending")
		}
	}
	label = "pr#" + strconv.Itoa(pr.Number)
	if len(parts) > 0 {
		label += " " + strings.Join(parts, ", ")
	}
	return label, state
}
