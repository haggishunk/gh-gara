package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func getCurrentBranch() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("getting current branch: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// parseTicketIDs finds Jira/Linear-style ticket IDs (e.g. PROJ-123) in a branch name.
func parseTicketIDs(branch string) []ticketRef {
	re := regexp.MustCompile(`[A-Z]+-\d+`)
	matches := re.FindAllString(branch, -1)
	refs := make([]ticketRef, 0, len(matches))
	for _, m := range matches {
		refs = append(refs, ticketRef{ID: m})
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

	commits, err := getCommitLog(defaultBranch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not get commit log: %v\n", err)
		commits = ""
	}

	fmt.Fprintf(os.Stderr, "branch:  %s\n", branch)
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

	if err := createPR(pr.Title, pr.Body, branch, defaultBranch); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
