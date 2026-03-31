# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

`gh-gara` is a GitHub CLI (`gh`) extension written in Go that automates GitHub PR creation. It uses the [`go-gh`](https://github.com/cli/go-gh) library to interact with the GitHub API via the authenticated `gh` session.

**Core workflow:**
1. Parse the current git branch name for Jira and/or Linear ticket references
2. If tickets are found, fetch their details to populate a rich PR title and description
3. If no tickets are found, fall back to summarizing commits — using the crux of the changes as the title and all relevant changes in the description
4. Create the PR with the generated content

The extension must be run through `gh` (e.g., `gh gara`) rather than directly — it relies on `gh`'s ambient authentication.

## Commands

```bash
# Build
go build

# Run (requires gh CLI installed and authenticated)
gh gara

# Run tests
go test ./...

# Update dependencies
go mod tidy
```

## Release

Releases are triggered by pushing a `v*` tag. The `cli/gh-extension-precompile@v2` GitHub Action handles cross-platform binary builds and attests artifacts automatically.

```bash
git tag v0.1.0
git push origin v0.1.0
```

## Architecture

Currently all logic lives in `main.go`. As the extension grows, expected areas of concern:

- **Branch parsing** — extract Jira ticket keys (e.g. `PROJECT-123`) and Linear ticket IDs from the branch name using regex
- **Jira integration** — fetch issue title, description, and type via the Jira REST API
- **Linear integration** — fetch issue title and description via the Linear API (GraphQL)
- **Commit summarization** — when no tickets are found, walk commits between the branch and its base to derive a PR title and full description
- **PR creation** — use `go-gh`'s REST client to create the PR with assembled content

`api.DefaultRESTClient()` picks up credentials from the `gh` auth context — no explicit token handling needed for GitHub API calls.

For more `go-gh` usage patterns, see the [go-gh examples](https://github.com/cli/go-gh/blob/trunk/example_gh_test.go).
