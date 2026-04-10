package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/repository"
)

type ticketRef struct {
	ID string
}

type prContent struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Matches Claude's --output-format json response envelope.
type claudeResult struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
}

// gitRunFunc executes a git subcommand and returns trimmed stdout.
type gitRunFunc func(args ...string) (string, error)

// runGit is the default gitRunFunc used in production.
func runGit(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	return strings.TrimSpace(string(out)), err
}

func getCurrentBranch() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("getting current branch: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// parseTicketIDs finds Jira/Linear-style ticket IDs (e.g. PROJ-123) in a branch name.
// Matching is case-insensitive; IDs are normalized to uppercase.
func parseTicketIDs(branch string) []ticketRef {
	re := regexp.MustCompile(`(?i)[A-Z]+-\d+`)
	matches := re.FindAllString(branch, -1)
	refs := make([]ticketRef, 0, len(matches))
	for _, m := range matches {
		refs = append(refs, ticketRef{ID: strings.ToUpper(m)})
	}
	return refs
}

func getDefaultBranch() (string, error) {
	repo, err := repository.Current()
	if err != nil {
		return "main", nil
	}
	client, err := api.DefaultRESTClient()
	if err != nil {
		return "main", nil
	}
	var info struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := client.Get(fmt.Sprintf("repos/%s/%s", repo.Owner, repo.Name), &info); err != nil || info.DefaultBranch == "" {
		return "main", nil
	}
	return info.DefaultBranch, nil
}

// getUpstreamBranch returns the local name of the configured upstream for the
// current branch (stripping the remote prefix). Returns "" when no upstream is
// configured, when the upstream resolves to the current branch itself, or when
// the upstream local name is "HEAD" (e.g. origin/HEAD).
func getUpstreamBranch(currentBranch string, run gitRunFunc) string {
	out, err := run("rev-parse", "--abbrev-ref", "@{upstream}")
	if err != nil {
		return ""
	}
	upstream := out
	// Strip remote prefix (e.g. "origin/develop" → "develop")
	parts := strings.SplitN(upstream, "/", 2)
	if len(parts) == 2 {
		upstream = parts[1]
	}
	if upstream == "" || upstream == "HEAD" || upstream == currentBranch {
		return ""
	}
	return upstream
}

// listRemoteBranches returns all remote-tracking branch refs, excluding HEAD
// pointer entries (both "origin/HEAD" and "origin/HEAD -> origin/main" forms).
func listRemoteBranches(run gitRunFunc) ([]string, error) {
	out, err := run("branch", "-r", "--format=%(refname:short)")
	if err != nil {
		return nil, fmt.Errorf("listing remote branches: %w", err)
	}
	var branches []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "->") {
			continue
		}
		// Skip origin/HEAD — not a real branch.
		parts := strings.SplitN(line, "/", 2)
		localName := line
		if len(parts) == 2 {
			localName = parts[1]
		}
		if localName == "HEAD" {
			continue
		}
		branches = append(branches, line)
	}
	return branches, nil
}

// listLocalBranches returns all local branch names, excluding HEAD.
func listLocalBranches(run gitRunFunc) ([]string, error) {
	out, err := run("branch", "--format=%(refname:short)")
	if err != nil {
		return nil, fmt.Errorf("listing local branches: %w", err)
	}
	var branches []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "HEAD" {
			continue
		}
		branches = append(branches, line)
	}
	return branches, nil
}

// mergeBaseCommitCount returns the number of commits between the merge-base of
// HEAD and the given remote branch ref. Returns math.MaxInt when the
// merge-base cannot be computed (unrelated histories, shallow clone, etc.).
func mergeBaseCommitCount(remoteBranchRef string, run gitRunFunc) int {
	mergeBase, err := run("merge-base", "HEAD", remoteBranchRef)
	if err != nil {
		return math.MaxInt
	}

	countOut, err := run("rev-list", "--count", mergeBase+"..HEAD")
	if err != nil {
		return math.MaxInt
	}
	var count int
	if _, err := fmt.Sscanf(countOut, "%d", &count); err != nil {
		return math.MaxInt
	}
	return count
}

// nearestRemoteBranch finds the nearest ancestor of HEAD by considering both
// remote-tracking refs and local branches. Scoring both ensures that a local
// branch whose remote tracking ref is stale (e.g. origin/deploy-prod points to
// an older commit than local deploy-prod) still scores correctly. currentBranch
// is excluded from candidates. Returns defaultBranch if no suitable candidate
// is found.
func nearestRemoteBranch(currentBranch, defaultBranch string, run gitRunFunc) string {
	type candidate struct {
		ref       string // git ref used for merge-base (e.g. "origin/deploy-prod" or "deploy-prod")
		localName string // branch name to return as the PR base
	}

	remotes, _ := listRemoteBranches(run)
	locals, _ := listLocalBranches(run)

	var candidates []candidate
	for _, ref := range remotes {
		parts := strings.SplitN(ref, "/", 2)
		localName := ref
		if len(parts) == 2 {
			localName = parts[1]
		}
		if localName == "HEAD" || localName == currentBranch {
			continue
		}
		candidates = append(candidates, candidate{ref: ref, localName: localName})
	}
	for _, name := range locals {
		if name == currentBranch {
			continue
		}
		candidates = append(candidates, candidate{ref: name, localName: name})
	}

	if len(candidates) == 0 {
		return defaultBranch
	}

	headHash, err := run("rev-parse", "HEAD")
	if err != nil {
		return defaultBranch
	}

	bestBranch := defaultBranch
	bestCount := math.MaxInt

	for _, c := range candidates {
		count := mergeBaseCommitCount(c.ref, run)
		if count == math.MaxInt {
			continue
		}

		// Skip branches whose merge-base is HEAD itself (descendant branches).
		mbHash, err := run("merge-base", "HEAD", c.ref)
		if err != nil {
			continue
		}
		if mbHash == headHash {
			continue
		}

		if count < bestCount {
			bestCount = count
			bestBranch = c.localName
		}
	}

	return bestBranch
}

// detectBaseBranch implements two-stage base branch detection:
//  1. Configured git upstream (stripped of remote prefix)
//  2. Nearest remote ancestor by merge-base commit count
//
// Falls back to defaultBranch if neither stage produces a result.
func detectBaseBranch(currentBranch, defaultBranch string, run gitRunFunc) string {
	if upstream := getUpstreamBranch(currentBranch, run); upstream != "" {
		return upstream
	}
	return nearestRemoteBranch(currentBranch, defaultBranch, run)
}

func getCommitLog(base string) (string, error) {
	args := []string{"log", fmt.Sprintf("origin/%s..HEAD", base), "--format=%s%n%b"}
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		// Retry without origin/ prefix (local-only branches)
		args[1] = fmt.Sprintf("%s..HEAD", base)
		out, err = exec.Command("git", args...).Output()
		if err != nil {
			return "", fmt.Errorf("getting commit log: %w", err)
		}
	}
	return strings.TrimSpace(string(out)), nil
}

func buildPrompt(branch string, tickets []ticketRef, commits string) string {
	var sb strings.Builder

	sb.WriteString("You are generating a GitHub pull request title and description.\n")
	sb.WriteString("Respond with ONLY a JSON object — no markdown, no explanation, no code fences:\n")
	sb.WriteString(`{"title": "...", "body": "..."}`)
	sb.WriteString("\n\n")
	sb.WriteString(fmt.Sprintf("Current branch: %s\n\n", branch))

	if len(tickets) > 0 {
		sb.WriteString("Ticket IDs found in the branch name:\n")
		for _, t := range tickets {
			sb.WriteString(fmt.Sprintf("  %s\n", t.ID))
		}
		sb.WriteString("\nUse your available MCP tools to look up these tickets in Jira or Linear. ")
		sb.WriteString("Use the ticket title, type, and description to produce:\n")
		sb.WriteString("  - title: a concise PR title that reflects the ticket's purpose\n")
		sb.WriteString("  - body: a description covering what the ticket is about, what changed, and any relevant context\n\n")
	} else {
		sb.WriteString("No ticket IDs found in the branch name.\n")
		sb.WriteString("Use the commit history to produce:\n")
		sb.WriteString("  - title: a concise summary capturing the crux of the changes\n")
		sb.WriteString("  - body: a description covering all relevant changes in detail\n\n")
	}

	if commits != "" {
		sb.WriteString("Commit history (newest first):\n")
		sb.WriteString(commits)
		sb.WriteString("\n")
	} else {
		sb.WriteString("(No commits found between this branch and the base branch.)\n")
	}

	return sb.String()
}

func invokeClaudeAgent(prompt string) (string, error) {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return "", fmt.Errorf("claude CLI not found in PATH — is Claude Code installed?")
	}

	cmd := exec.Command(claudePath, "--print", "--output-format", "json")
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude exited with error: %w\nstderr: %s", err, stderr.String())
	}

	return stdout.String(), nil
}

func parsePRContent(claudeOutput string) (*prContent, error) {
	var wrapper claudeResult
	if err := json.Unmarshal([]byte(claudeOutput), &wrapper); err != nil {
		return nil, fmt.Errorf("parsing claude response envelope: %w\nraw output: %s", err, claudeOutput)
	}
	if wrapper.IsError {
		return nil, fmt.Errorf("claude returned an error: %s", wrapper.Result)
	}

	result := strings.TrimSpace(wrapper.Result)

	// Strip markdown code fences if Claude wrapped the JSON anyway
	if strings.HasPrefix(result, "```") {
		lines := strings.SplitN(result, "\n", 2)
		result = lines[1]
		if idx := strings.LastIndex(result, "```"); idx >= 0 {
			result = strings.TrimSpace(result[:idx])
		}
	}

	var pr prContent
	if err := json.Unmarshal([]byte(result), &pr); err != nil {
		return nil, fmt.Errorf("parsing PR JSON from claude result: %w\nresult was: %s", err, result)
	}

	return &pr, nil
}

func createPR(title, body, head, base string) error {
	repo, err := repository.Current()
	if err != nil {
		return fmt.Errorf("detecting repository: %w", err)
	}
	client, err := api.DefaultRESTClient()
	if err != nil {
		return fmt.Errorf("creating API client: %w", err)
	}

	payload, err := json.Marshal(struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}{title, body, head, base})
	if err != nil {
		return fmt.Errorf("marshaling PR payload: %w", err)
	}

	var result struct {
		HTMLURL string `json:"html_url"`
	}

	if err := client.Post(fmt.Sprintf("repos/%s/%s/pulls", repo.Owner, repo.Name), bytes.NewReader(payload), &result); err != nil {
		return fmt.Errorf("creating PR: %w", err)
	}

	fmt.Printf("PR created: %s\n", result.HTMLURL)
	return nil
}

func main() {
	dryRun := len(os.Args) > 1 && os.Args[1] == "--dry-run"

	branch, err := getCurrentBranch()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	tickets := parseTicketIDs(branch)

	defaultBranch, err := getDefaultBranch()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	baseBranch := detectBaseBranch(branch, defaultBranch, runGit)

	commits, err := getCommitLog(baseBranch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not get commit log: %v\n", err)
		commits = ""
	}

	fmt.Fprintf(os.Stderr, "branch:  %s\n", branch)
	fmt.Fprintf(os.Stderr, "base:    %s\n", baseBranch)
	if len(tickets) > 0 {
		ids := make([]string, len(tickets))
		for i, t := range tickets {
			ids[i] = t.ID
		}
		fmt.Fprintf(os.Stderr, "tickets: %s\n", strings.Join(ids, ", "))
	} else {
		fmt.Fprintf(os.Stderr, "tickets: none found, using commit history\n")
	}
	fmt.Fprintln(os.Stderr, "invoking Claude...")

	prompt := buildPrompt(branch, tickets, commits)
	claudeOutput, err := invokeClaudeAgent(prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	pr, err := parsePRContent(claudeOutput)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("title: %s\n\n", pr.Title)
	fmt.Printf("body:\n%s\n", pr.Body)

	if dryRun {
		fmt.Fprintln(os.Stderr, "\n(dry run — PR not created)")
		return
	}

	if err := createPR(pr.Title, pr.Body, branch, baseBranch); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
