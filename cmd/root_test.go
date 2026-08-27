package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/k1LoW/go-github-client/v79/factory"
)

func TestGitHubAPIEndToEnd(t *testing.T) {
	if os.Getenv("GH_SHARE_E2E") == "" {
		t.Skip("set GH_SHARE_E2E=1 to run the GitHub API end-to-end test")
	}

	const repository = "k1LoW/gh-share"
	owner, repo, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || repo == "" {
		t.Fatalf("invalid repository: %s", repository)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	c, err := factory.NewGithubClient()
	if err != nil {
		t.Fatalf("create GitHub client: %v", err)
	}

	branch := fmt.Sprintf("gh-share-e2e-%d", time.Now().UnixNano())
	previousBranch := shareBranch
	shareBranch = branch
	t.Cleanup(func() {
		shareBranch = previousBranch
		_, _ = c.Git.DeleteRef(context.Background(), owner, repo, "heads/"+branch)
	})

	input := filepath.Join(t.TempDir(), "report.html")
	if err := os.WriteFile(input, []byte("<h1>E2E</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC().Format("20060102-150405")
	files, err := payloadFiles(input, false, ts)
	if err != nil {
		t.Fatalf("create payload: %v", err)
	}
	files[".gh-share-payload-ref"] = []byte(ts + " file report.html\n")

	sha, err := commitPayload(ctx, c, owner, repo, branch, files, func(string) {})
	if err != nil {
		t.Fatalf("commit payload: %v", err)
	}
	if sha == "" {
		t.Fatal("commit payload returned an empty SHA")
	}

	ref, _, err := c.Git.GetRef(ctx, owner, repo, "heads/"+branch)
	if err != nil {
		t.Fatalf("get staging branch: %v", err)
	}
	if got := ref.GetObject().GetSHA(); got != sha {
		t.Fatalf("staging branch points to %s, want %s", got, sha)
	}

	run, err := waitForRun(ctx, c, owner, repo, sha, func(string) {})
	if err != nil {
		t.Fatalf("wait for workflow: %v", err)
	}
	if run.GetConclusion() != "success" {
		t.Fatalf("workflow conclusion = %q, want success", run.GetConclusion())
	}

	artifact, err := artifactURL(ctx, c, owner, repo, run.GetID())
	if err != nil {
		t.Fatalf("get artifact URL: %v", err)
	}
	if !strings.Contains(artifact, fmt.Sprintf("/runs/%d/artifacts/", run.GetID())) {
		t.Fatalf("artifact URL = %q, want run ID %d", artifact, run.GetID())
	}
	artifacts, _, err := c.Actions.ListWorkflowRunArtifacts(ctx, owner, repo, run.GetID(), nil)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(artifacts.Artifacts) != 1 || artifacts.Artifacts[0].GetName() != "report.html" {
		t.Fatalf("artifact name = %#v, want report.html", artifacts.Artifacts)
	}
}

func TestPayloadFilesFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	input := filepath.Join(dir, "report.html")
	if err := os.WriteFile(input, []byte("<h1>Report</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}

	files, err := payloadFiles(input, false, "20260827-120000")
	if err != nil {
		t.Fatal(err)
	}

	wantPath := "gh-share-payload/20260827-120000/report.html"
	if string(files[wantPath]) != "<h1>Report</h1>" {
		t.Fatalf("payloadFiles() = %#v, want %q at %q", files, "<h1>Report</h1>", wantPath)
	}
}

func TestPayloadFilesDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "nested", "report.html")
	if err := os.WriteFile(input, []byte("<h1>Report</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}

	files, err := payloadFiles(dir, true, "20260827-120000")
	if err != nil {
		t.Fatal(err)
	}

	wantPath := "gh-share-payload/20260827-120000/nested/report.html"
	if string(files[wantPath]) != "<h1>Report</h1>" {
		t.Fatalf("payloadFiles() = %#v, want %q at %q", files, "<h1>Report</h1>", wantPath)
	}
}

func TestFormatSummary(t *testing.T) {
	t.Parallel()

	got := formatSummary(
		"https://github.com/k1LoW/gh-share/tree/gh-share-staging",
		"deleted",
		"https://github.com/k1LoW/gh-share/commit/abc123",
		"https://github.com/k1LoW/gh-share/actions/runs/123",
	)

	for _, want := range []string{
		"╔",
		"╚",
		"Branch:   https://github.com/k1LoW/gh-share/tree/gh-share-staging (deleted)",
		"Commit:   https://github.com/k1LoW/gh-share/commit/abc123",
		"Workflow: https://github.com/k1LoW/gh-share/actions/runs/123",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatSummary() does not contain %q:\n%s", want, got)
		}
	}
}

func TestConfirmPurge(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	ok, err := confirmPurge(strings.NewReader("yes\n"), &out, "k1LoW", "gh-share", 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("confirmPurge() = false, want true")
	}
	if !strings.Contains(out.String(), "Purge 2 gh-share workflow run(s)") {
		t.Fatalf("confirmation prompt = %q", out.String())
	}

	out.Reset()
	ok, err = confirmPurge(strings.NewReader("n\n"), &out, "k1LoW", "gh-share", 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("confirmPurge() = true, want false")
	}
	if !strings.Contains(out.String(), "Purge cancelled.") {
		t.Fatalf("cancellation output = %q", out.String())
	}
}
