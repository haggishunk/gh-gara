package main

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// mockRunner returns a gitRunFunc that responds to exact arg-string keys.
// Keys are formed by joining args with a single space.
// An optional fallback func can handle unregistered calls.
func mockRunner(responses map[string]string) gitRunFunc {
	return func(args ...string) (string, error) {
		key := strings.Join(args, " ")
		if resp, ok := responses[key]; ok {
			return resp, nil
		}
		return "", fmt.Errorf("unexpected git call: git %s", key)
	}
}

// errRunner always returns an error for every call.
func errRunner(args ...string) (string, error) {
	return "", fmt.Errorf("git %s: simulated error", strings.Join(args, " "))
}

// --- listRemoteBranches ---

func TestListRemoteBranches_FiltersHEAD(t *testing.T) {
	run := mockRunner(map[string]string{
		"branch -r --format=%(refname:short)": "origin/HEAD\norigin/main\norigin/feature/foo",
	})
	branches, err := listRemoteBranches(run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, b := range branches {
		parts := strings.SplitN(b, "/", 2)
		local := b
		if len(parts) == 2 {
			local = parts[1]
		}
		if local == "HEAD" {
			t.Errorf("listRemoteBranches returned HEAD entry: %q", b)
		}
	}
	if len(branches) != 2 {
		t.Errorf("expected 2 branches, got %d: %v", len(branches), branches)
	}
}

func TestListRemoteBranches_FiltersArrowLines(t *testing.T) {
	// Some git versions may still emit arrow notation even with --format.
	run := mockRunner(map[string]string{
		"branch -r --format=%(refname:short)": "origin/HEAD -> origin/main\norigin/main\norigin/develop",
	})
	branches, err := listRemoteBranches(run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, b := range branches {
		if strings.Contains(b, "->") {
			t.Errorf("listRemoteBranches returned arrow line: %q", b)
		}
	}
	if len(branches) != 2 {
		t.Errorf("expected 2 branches, got %d: %v", len(branches), branches)
	}
}

func TestListRemoteBranches_Empty(t *testing.T) {
	run := mockRunner(map[string]string{
		"branch -r --format=%(refname:short)": "",
	})
	branches, err := listRemoteBranches(run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(branches) != 0 {
		t.Errorf("expected 0 branches, got %d", len(branches))
	}
}

func TestListRemoteBranches_Error(t *testing.T) {
	_, err := listRemoteBranches(errRunner)
	if err == nil {
		t.Error("expected error, got nil")
	}
}

// --- listLocalBranches ---

func TestListLocalBranches_Normal(t *testing.T) {
	run := mockRunner(map[string]string{
		"branch --format=%(refname:short)": "main\ndevelop\nfeature/foo",
	})
	branches, err := listLocalBranches(run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(branches) != 3 {
		t.Errorf("expected 3 branches, got %d: %v", len(branches), branches)
	}
}

func TestListLocalBranches_Empty(t *testing.T) {
	run := mockRunner(map[string]string{
		"branch --format=%(refname:short)": "",
	})
	branches, err := listLocalBranches(run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(branches) != 0 {
		t.Errorf("expected 0 branches, got %d", len(branches))
	}
}

func TestListLocalBranches_Error(t *testing.T) {
	_, err := listLocalBranches(errRunner)
	if err == nil {
		t.Error("expected error, got nil")
	}
}

// --- getUpstreamBranch ---

func TestGetUpstreamBranch_Normal(t *testing.T) {
	run := mockRunner(map[string]string{
		"rev-parse --abbrev-ref @{upstream}": "origin/develop",
	})
	got := getUpstreamBranch("feature/abc", run)
	if got != "develop" {
		t.Errorf("expected %q, got %q", "develop", got)
	}
}

func TestGetUpstreamBranch_HEAD(t *testing.T) {
	// When the upstream resolves to origin/HEAD the local name is "HEAD",
	// which is not a valid branch — must be rejected.
	run := mockRunner(map[string]string{
		"rev-parse --abbrev-ref @{upstream}": "origin/HEAD",
	})
	got := getUpstreamBranch("feature/abc", run)
	if got != "" {
		t.Errorf("expected empty string for HEAD upstream, got %q", got)
	}
}

func TestGetUpstreamBranch_SameBranch(t *testing.T) {
	run := mockRunner(map[string]string{
		"rev-parse --abbrev-ref @{upstream}": "origin/feature/abc",
	})
	got := getUpstreamBranch("feature/abc", run)
	if got != "" {
		t.Errorf("expected empty string when upstream == current branch, got %q", got)
	}
}

func TestGetUpstreamBranch_NoUpstream(t *testing.T) {
	got := getUpstreamBranch("feature/abc", errRunner)
	if got != "" {
		t.Errorf("expected empty string on error, got %q", got)
	}
}

// --- mergeBaseCommitCount ---

func TestMergeBaseCommitCount_Normal(t *testing.T) {
	run := mockRunner(map[string]string{
		"merge-base HEAD origin/main": "abc123",
		"rev-list --count abc123..HEAD": "3",
	})
	count := mergeBaseCommitCount("origin/main", run)
	if count != 3 {
		t.Errorf("expected 3, got %d", count)
	}
}

func TestMergeBaseCommitCount_ErrorMergeBase(t *testing.T) {
	count := mergeBaseCommitCount("origin/main", errRunner)
	if count != math.MaxInt {
		t.Errorf("expected MaxInt on error, got %d", count)
	}
}

func TestMergeBaseCommitCount_ErrorRevList(t *testing.T) {
	run := mockRunner(map[string]string{
		"merge-base HEAD origin/main": "abc123",
		// rev-list is intentionally absent → errRunner fallthrough handled by mock
	})
	count := mergeBaseCommitCount("origin/main", run)
	if count != math.MaxInt {
		t.Errorf("expected MaxInt when rev-list fails, got %d", count)
	}
}

// --- nearestRemoteBranch ---

func TestNearestRemoteBranch_SkipsHEAD(t *testing.T) {
	// origin/HEAD must be skipped; origin/main should win.
	headHash := "deadbeef"
	mainMergeBase := "aabbccdd"
	run := mockRunner(map[string]string{
		"branch -r --format=%(refname:short)": "origin/HEAD\norigin/main\norigin/feature/current",
		"rev-parse HEAD":                      headHash,
		// origin/HEAD is filtered before merge-base is called (via listRemoteBranches)
		"merge-base HEAD origin/main":         mainMergeBase,
		"rev-list --count " + mainMergeBase + "..HEAD": "2",
	})
	got := nearestRemoteBranch("feature/current", "main", run)
	if got != "main" {
		t.Errorf("expected %q, got %q", "main", got)
	}
}

func TestNearestRemoteBranch_SkipsCurrentBranch(t *testing.T) {
	headHash := "deadbeef"
	mainMergeBase := "aabbccdd"
	run := mockRunner(map[string]string{
		"branch -r --format=%(refname:short)": "origin/main\norigin/mybranch",
		"rev-parse HEAD":                      headHash,
		"merge-base HEAD origin/main":         mainMergeBase,
		"rev-list --count " + mainMergeBase + "..HEAD": "1",
		// origin/mybranch should be skipped (same as currentBranch)
	})
	got := nearestRemoteBranch("mybranch", "main", run)
	if got != "main" {
		t.Errorf("expected %q, got %q", "main", got)
	}
}

func TestNearestRemoteBranch_SkipsDescendants(t *testing.T) {
	// A descendant branch has merge-base == HEAD — should be skipped.
	headHash := "deadbeef"
	run := mockRunner(map[string]string{
		"branch -r --format=%(refname:short)": "origin/main\norigin/downstream",
		"rev-parse HEAD":                      headHash,
		// main: valid ancestor
		"merge-base HEAD origin/main":     "aabbcc",
		"rev-list --count aabbcc..HEAD":   "2",
		// downstream: merge-base == HEAD → descendant, skip
		"merge-base HEAD origin/downstream": headHash,
	})
	got := nearestRemoteBranch("feature/x", "main", run)
	if got != "main" {
		t.Errorf("expected %q, got %q", "main", got)
	}
}

func TestNearestRemoteBranch_FallsBackToDefault(t *testing.T) {
	// No usable remote branches — fall back to default.
	run := mockRunner(map[string]string{
		"branch -r --format=%(refname:short)": "",
		"rev-parse HEAD":                      "deadbeef",
	})
	got := nearestRemoteBranch("feature/x", "main", run)
	if got != "main" {
		t.Errorf("expected %q, got %q", "main", got)
	}
}

func TestNearestRemoteBranch_LocalBranchWinsOverStaleRemote(t *testing.T) {
	// Simulates the scenario where origin/deploy-prod is stale (its remote
	// tracking ref points far behind HEAD) but the local deploy-prod branch is
	// a direct 2-commit ancestor. The local ref should produce a lower count
	// and win over origin/develop which has an intermediate distance.
	headHash := "993a74a1"
	localDeployMergeBase := "b52ef160" // local deploy-prod is a direct ancestor
	staleRemoteMergeBase := "00000001" // origin/deploy-prod hasn't been fetched recently
	developMergeBase := "cccccccc"

	run := mockRunner(map[string]string{
		"branch -r --format=%(refname:short)": "origin/deploy-prod\norigin/develop",
		"branch --format=%(refname:short)":    "deploy-prod\ndevelop",
		"rev-parse HEAD":                      headHash,
		// origin/deploy-prod: stale → large count
		"merge-base HEAD origin/deploy-prod":                    staleRemoteMergeBase,
		"rev-list --count " + staleRemoteMergeBase + "..HEAD":   "50",
		// local deploy-prod: direct ancestor → small count
		"merge-base HEAD deploy-prod":                           localDeployMergeBase,
		"rev-list --count " + localDeployMergeBase + "..HEAD":   "2",
		// origin/develop and local develop: intermediate distance
		"merge-base HEAD origin/develop":                        developMergeBase,
		"merge-base HEAD develop":                               developMergeBase,
		"rev-list --count " + developMergeBase + "..HEAD":       "10",
	})
	got := nearestRemoteBranch("feature/my-branch", "main", run)
	if got != "deploy-prod" {
		t.Errorf("expected %q (local branch beats stale remote), got %q", "deploy-prod", got)
	}
}

// --- detectBaseBranch ---

func TestDetectBaseBranch_UsesUpstream(t *testing.T) {
	run := mockRunner(map[string]string{
		"rev-parse --abbrev-ref @{upstream}": "origin/develop",
	})
	got := detectBaseBranch("feature/abc", "main", run)
	if got != "develop" {
		t.Errorf("expected %q, got %q", "develop", got)
	}
}

func TestDetectBaseBranch_HeadUpstreamFallsToNearest(t *testing.T) {
	headHash := "deadbeef"
	mergeBase := "aabbccdd"
	run := mockRunner(map[string]string{
		// Upstream resolves to HEAD — invalid, must not use it.
		"rev-parse --abbrev-ref @{upstream}":            "origin/HEAD",
		"branch -r --format=%(refname:short)":           "origin/HEAD\norigin/main",
		"rev-parse HEAD":                                headHash,
		"merge-base HEAD origin/main":                   mergeBase,
		"rev-list --count " + mergeBase + "..HEAD":      "3",
	})
	got := detectBaseBranch("feature/abc", "main", run)
	if got != "main" {
		t.Errorf("expected %q, got %q", "main", got)
	}
}

func TestDetectBaseBranch_NoUpstreamFallsToNearest(t *testing.T) {
	headHash := "deadbeef"
	mergeBase := "aabbccdd"
	run := mockRunner(map[string]string{
		// No upstream configured.
		"branch -r --format=%(refname:short)":      "origin/main\norigin/feature/abc",
		"rev-parse HEAD":                           headHash,
		"merge-base HEAD origin/main":              mergeBase,
		"rev-list --count " + mergeBase + "..HEAD": "2",
	})
	got := detectBaseBranch("feature/abc", "main", run)
	if got != "main" {
		t.Errorf("expected %q, got %q", "main", got)
	}
}

// --- appendTicketSuffix ---

func TestAppendTicketSuffix_NoTickets(t *testing.T) {
	got := appendTicketSuffix("Fix login bug", nil)
	if got != "Fix login bug" {
		t.Errorf("expected unchanged title, got %q", got)
	}
}

func TestAppendTicketSuffix_SingleTicket(t *testing.T) {
	got := appendTicketSuffix("Fix login bug", []ticketRef{{ID: "PROJ-123"}})
	want := "Fix login bug [PROJ-123]"
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

func TestAppendTicketSuffix_MultipleTickets(t *testing.T) {
	got := appendTicketSuffix("Fix login bug", []ticketRef{{ID: "PROJ-123"}, {ID: "PROJ-456"}})
	want := "Fix login bug [PROJ-123, PROJ-456]"
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}
