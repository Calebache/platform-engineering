package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v90/github"
)

// gitHost is the slice of GitHub the tools use; a fake implements it in tests.
type gitHost interface {
	recentCommits(ctx context.Context, paths []string, limit int) ([]Commit, error)
	readFile(ctx context.Context, path string) (string, error)
	createPullRequest(ctx context.Context, pr PullRequestInput) (*PullRequestResult, error)
	pullRequest(ctx context.Context, number int) (*PullRequestStatus, error)
}

type Commit struct {
	SHA     string       `json:"sha"`
	Author  string       `json:"author"`
	Date    string       `json:"date"`
	Message string       `json:"message"`
	Files   []CommitFile `json:"files,omitempty"`
}

type CommitFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Patch  string `json:"patch,omitempty"`
}

type FileChange struct {
	Path    string `json:"path" jsonschema:"repository path, must be under gitops/workloads/"`
	Content string `json:"content" jsonschema:"complete new file content"`
}

type PullRequestInput struct {
	Branch string       `json:"branch" jsonschema:"new branch name; must start with agent/"`
	Title  string       `json:"title" jsonschema:"pull request title"`
	Body   string       `json:"body" jsonschema:"hypothesis, evidence, change and verification plan"`
	Files  []FileChange `json:"files" jsonschema:"files to create or replace"`
}

type PullRequestResult struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Branch string `json:"branch"`
}

// PullRequestStatus is what the agent needs to record human approval:
// whether its PR was merged, by whom and when.
type PullRequestStatus struct {
	Number   int    `json:"number"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Branch   string `json:"branch"`
	State    string `json:"state"`
	Merged   bool   `json:"merged"`
	MergedBy string `json:"merged_by,omitempty"`
	MergedAt string `json:"merged_at,omitempty"`
	MergeSHA string `json:"merge_commit_sha,omitempty"`
}

var errNotAgentPR = errors.New("only pull requests from agent/* branches can be read")

type githubBackend struct {
	client      *github.Client
	owner, repo string
	base        string
}

// newGitHubFromEnv authenticates as the MCP server's GitHub App. Installation
// tokens are short-lived and refreshed by the transport; only the app key is
// long-lived, and it never leaves this process.
func newGitHubFromEnv(_ context.Context) (*githubBackend, error) {
	repo := envOr("GITHUB_REPOSITORY", "Calebache/platform-engineering")
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return nil, fmt.Errorf("GITHUB_REPOSITORY %q must be owner/name", repo)
	}
	appID, err := strconv.ParseInt(os.Getenv("GITHUB_APP_ID"), 10, 64)
	if err != nil {
		return nil, errors.New("GITHUB_APP_ID must be set")
	}
	instID, err := strconv.ParseInt(os.Getenv("GITHUB_APP_INSTALLATION_ID"), 10, 64)
	if err != nil {
		return nil, errors.New("GITHUB_APP_INSTALLATION_ID must be set")
	}
	key, err := os.ReadFile(envOr("GITHUB_APP_PRIVATE_KEY_FILE", "/var/run/secrets/github-app/private-key"))
	if err != nil {
		return nil, fmt.Errorf("read GitHub App key: %w", err)
	}
	tr, err := ghinstallation.New(http.DefaultTransport, appID, instID, key)
	if err != nil {
		return nil, fmt.Errorf("GitHub App transport: %w", err)
	}
	client, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: tr, Timeout: 20 * time.Second}))
	if err != nil {
		return nil, fmt.Errorf("GitHub client: %w", err)
	}
	return &githubBackend{client: client, owner: owner, repo: name, base: envOr("GITHUB_BASE_BRANCH", "main")}, nil
}

func (g *githubBackend) recentCommits(ctx context.Context, paths []string, limit int) ([]Commit, error) {
	seen := map[string]*github.RepositoryCommit{}
	for _, p := range paths {
		list, _, err := g.client.Repositories.ListCommits(ctx, g.owner, g.repo, &github.CommitsListOptions{
			SHA: g.base, Path: p, ListOptions: github.ListOptions{PerPage: limit},
		})
		if err != nil {
			return nil, fmt.Errorf("list commits for %s: %w", p, err)
		}
		for _, c := range list {
			seen[c.GetSHA()] = c
		}
	}
	shas := make([]string, 0, len(seen))
	for sha := range seen {
		shas = append(shas, sha)
	}
	sort.Slice(shas, func(i, j int) bool {
		return seen[shas[i]].GetCommit().GetCommitter().GetDate().After(seen[shas[j]].GetCommit().GetCommitter().GetDate().Time)
	})
	if len(shas) > limit {
		shas = shas[:limit]
	}

	var out []Commit
	for _, sha := range shas {
		full, _, err := g.client.Repositories.GetCommit(ctx, g.owner, g.repo, sha, nil)
		if err != nil {
			return nil, fmt.Errorf("get commit %s: %w", sha, err)
		}
		c := Commit{
			SHA:     sha,
			Author:  full.GetCommit().GetAuthor().GetName(),
			Date:    full.GetCommit().GetCommitter().GetDate().UTC().Format(time.RFC3339),
			Message: truncate(full.GetCommit().GetMessage(), 500),
		}
		for _, f := range full.Files {
			if !underAny(f.GetFilename(), paths) {
				continue
			}
			c.Files = append(c.Files, CommitFile{Path: f.GetFilename(), Status: f.GetStatus(), Patch: redact(truncate(f.GetPatch(), 2000))})
		}
		out = append(out, c)
	}
	return out, nil
}

func underAny(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(path, strings.TrimSuffix(p, "/")+"/") || path == p {
			return true
		}
	}
	return false
}

func (g *githubBackend) readFile(ctx context.Context, path string) (string, error) {
	f, _, _, err := g.client.Repositories.GetContents(ctx, g.owner, g.repo, path, &github.RepositoryContentGetOptions{Ref: g.base})
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	if f == nil {
		return "", fmt.Errorf("%s is a directory", path)
	}
	return f.GetContent()
}

// createPullRequest branches from the base, commits each file, and opens a
// PR. Branch protection on the base branch is what makes the PR a proposal:
// it can't merge without CI and a human review.
func (g *githubBackend) createPullRequest(ctx context.Context, in PullRequestInput) (*PullRequestResult, error) {
	baseRef, _, err := g.client.Git.GetRef(ctx, g.owner, g.repo, "refs/heads/"+g.base)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", g.base, err)
	}
	if _, _, err := g.client.Git.CreateRef(ctx, g.owner, g.repo, github.CreateRef{
		Ref: "refs/heads/" + in.Branch, SHA: baseRef.GetObject().GetSHA(),
	}); err != nil {
		return nil, fmt.Errorf("create branch %s: %w", in.Branch, err)
	}

	for _, f := range in.Files {
		opts := &github.RepositoryContentFileOptions{
			Message: github.Ptr(fmt.Sprintf("agent: %s (%s)", in.Title, f.Path)),
			Content: []byte(f.Content),
			Branch:  github.Ptr(in.Branch),
		}
		existing, _, _, err := g.client.Repositories.GetContents(ctx, g.owner, g.repo, f.Path, &github.RepositoryContentGetOptions{Ref: in.Branch})
		if err == nil && existing != nil {
			opts.SHA = existing.SHA
			_, _, err = g.client.Repositories.UpdateFile(ctx, g.owner, g.repo, f.Path, opts)
		} else {
			_, _, err = g.client.Repositories.CreateFile(ctx, g.owner, g.repo, f.Path, opts)
		}
		if err != nil {
			return nil, fmt.Errorf("commit %s: %w", f.Path, err)
		}
	}

	pr, _, err := g.client.PullRequests.Create(ctx, g.owner, g.repo, github.CreatePullRequest{
		Title: github.Ptr(in.Title),
		Head:  in.Branch,
		Base:  g.base,
		Body:  github.Ptr(in.Body + "\n\n---\nOpened by the SRE agent through the MCP server. Requires CI and human review."),
	})
	if err != nil {
		return nil, fmt.Errorf("open pull request: %w", err)
	}
	return &PullRequestResult{Number: pr.GetNumber(), URL: pr.GetHTMLURL(), Branch: in.Branch}, nil
}

// pullRequest reads one PR, and only PRs the agent could have opened (agent/*
// branches), so this can't be used to browse unrelated work.
func (g *githubBackend) pullRequest(ctx context.Context, number int) (*PullRequestStatus, error) {
	pr, _, err := g.client.PullRequests.Get(ctx, g.owner, g.repo, number)
	if err != nil {
		return nil, fmt.Errorf("get pull request #%d: %w", number, err)
	}
	if !strings.HasPrefix(pr.GetHead().GetRef(), "agent/") {
		return nil, errNotAgentPR
	}
	st := &PullRequestStatus{
		Number: pr.GetNumber(), Title: pr.GetTitle(), URL: pr.GetHTMLURL(), Branch: pr.GetHead().GetRef(),
		State: pr.GetState(), Merged: pr.GetMerged(), MergedBy: pr.GetMergedBy().GetLogin(), MergeSHA: pr.GetMergeCommitSHA(),
	}
	if pr.MergedAt != nil {
		st.MergedAt = pr.GetMergedAt().UTC().Format(time.RFC3339)
	}
	if !st.Merged {
		st.MergeSHA = ""
	}
	return st, nil
}
