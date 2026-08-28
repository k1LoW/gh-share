package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v79/github"
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
	files[payloadRefPath] = []byte(ts + " file report.html\n")

	sha, err := commitPayload(ctx, c, owner, repo, branch, "Share payload", files, func(string) {})
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

	artifact, err := findArtifact(ctx, c, owner, repo, run.GetID())
	if err != nil {
		t.Fatalf("find artifact: %v", err)
	}
	if artifact.GetName() != "report.html" {
		t.Fatalf("artifact name = %q, want report.html", artifact.GetName())
	}
	url := artifactURL(owner, repo, run.GetID(), artifact.GetID())
	if !strings.Contains(url, fmt.Sprintf("/runs/%d/artifacts/%d", run.GetID(), artifact.GetID())) {
		t.Fatalf("artifact URL = %q, want run %d and artifact %d", url, run.GetID(), artifact.GetID())
	}

	record := artifactRecord{
		ArtifactURL: url,
		ArtifactID:  artifact.GetID(),
		RunID:       run.GetID(),
		Commit:      sha,
		PayloadDir:  payloadsDir + "/" + ts,
		Input:       "report.html",
		InputType:   "file",
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if err := recordArtifact(ctx, c, owner, repo, branch, record); err != nil {
		t.Fatalf("record artifact: %v", err)
	}

	content, _, _, err := c.Repositories.GetContents(ctx, owner, repo, artifactRecordPath(artifact.GetID()), &github.RepositoryContentGetOptions{Ref: branch})
	if err != nil {
		t.Fatalf("read artifact record: %v", err)
	}
	raw, err := content.GetContent()
	if err != nil {
		t.Fatalf("decode artifact record: %v", err)
	}
	var stored artifactRecord
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatalf("unmarshal artifact record: %v", err)
	}
	if stored.ArtifactURL != url || stored.PayloadDir != record.PayloadDir || stored.Input != "report.html" {
		t.Fatalf("artifact record = %#v, want URL %q and payload dir %q", stored, url, record.PayloadDir)
	}

	// The record commit must not start a second upload run, otherwise every
	// share would duplicate its artifact.
	recordRef, _, err := c.Git.GetRef(ctx, owner, repo, "heads/"+branch)
	if err != nil {
		t.Fatalf("get staging branch after record: %v", err)
	}
	recordSHA := recordRef.GetObject().GetSHA()
	if recordSHA == sha {
		t.Fatal("record did not create a commit")
	}
	time.Sleep(30 * time.Second)
	runs, _, err := c.Actions.ListWorkflowRunsByFileName(ctx, owner, repo, workflowFile, &github.ListWorkflowRunsOptions{Branch: branch, ListOptions: github.ListOptions{PerPage: 20}})
	if err != nil {
		t.Fatalf("list workflow runs after record: %v", err)
	}
	for _, r := range runs.WorkflowRuns {
		if r.GetHeadSHA() == recordSHA {
			t.Fatalf("record commit %s triggered workflow run %d", recordSHA, r.GetID())
		}
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

	wantPath := ".gh-share/payloads/20260827-120000/report.html"
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

	wantPath := ".gh-share/payloads/20260827-120000/nested/report.html"
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
	if !strings.Contains(out.String(), "Purge canceled.") {
		t.Fatalf("cancellation output = %q", out.String())
	}
}

func TestArtifactURL(t *testing.T) {
	t.Parallel()

	got := artifactURL("k1LoW", "gh-share", 123, 456)
	want := "https://github.com/k1LoW/gh-share/actions/runs/123/artifacts/456"
	if got != want {
		t.Errorf("artifactURL() = %q, want %q", got, want)
	}
}

func TestArtifactRecordPath(t *testing.T) {
	t.Parallel()

	got := artifactRecordPath(456)
	want := ".gh-share/artifacts/456.json"
	if got != want {
		t.Errorf("artifactRecordPath() = %q, want %q", got, want)
	}
}

func TestWorkflowMatchesLayout(t *testing.T) {
	t.Parallel()

	workflow := string(uploadWorkflow)

	// The workflow triggers on the payload ref alone. If the Go constants and
	// the embedded workflow drift apart, a share either never starts a run or
	// the artifact record commit starts a redundant one.
	if !strings.Contains(workflow, "- '"+payloadRefPath+"'") {
		t.Errorf("workflow does not trigger on %q:\n%s", payloadRefPath, workflow)
	}
	if !strings.Contains(workflow, "read -r PAYLOAD_DIR PAYLOAD_TYPE PAYLOAD_NAME < "+payloadRefPath) {
		t.Errorf("workflow does not read %q:\n%s", payloadRefPath, workflow)
	}
	if !strings.Contains(workflow, payloadsDir+"/${{ env.PAYLOAD_DIR }}/") {
		t.Errorf("workflow does not upload from %q:\n%s", payloadsDir, workflow)
	}
	if strings.Contains(workflow, artifactsDir) {
		t.Errorf("workflow references %q, so artifact records would trigger or be uploaded:\n%s", artifactsDir, workflow)
	}

	// Payloads live under a hidden directory, so upload-artifact must be told to
	// keep hidden files or a shared directory silently loses its dotfiles.
	if !strings.Contains(workflow, "include-hidden-files: true") {
		t.Errorf("workflow does not set include-hidden-files:\n%s", workflow)
	}
}
