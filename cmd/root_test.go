package cmd

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
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
	files[payloadRefPath] = payloadRef(shareID(), ts, "file", "report.html")

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

	// A file artifact is stored unarchived, so the download is the file itself.
	// Checking the bytes is what catches the payload being uploaded from the
	// wrong directory or arriving empty, which the artifact name alone hides.
	body := downloadArtifact(ctx, t, c, owner, repo, artifact.GetID())
	if string(body) != "<h1>E2E</h1>" {
		t.Fatalf("artifact content = %q, want %q", body, "<h1>E2E</h1>")
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
	if len(runs.WorkflowRuns) != 1 {
		t.Fatalf("branch produced %d workflow runs, want 1", len(runs.WorkflowRuns))
	}

	// Resharing rewrites nothing but the share ID in the payload ref. The payload
	// stays where it is, so this is what proves that the share ID alone re-triggers
	// the workflow and that the artifact is reproduced from the branch.
	reshareFiles := map[string][]byte{
		payloadRefPath: payloadRef(shareID(), path.Base(stored.PayloadDir), stored.InputType, stored.Input),
	}
	reshareSHA, err := commitPayload(ctx, c, owner, repo, branch, "Share payload", reshareFiles, func(string) {})
	if err != nil {
		t.Fatalf("commit reshare payload ref: %v", err)
	}
	reshareRun, err := waitForRun(ctx, c, owner, repo, reshareSHA, func(string) {})
	if err != nil {
		t.Fatalf("wait for reshare workflow: %v", err)
	}
	reshareArtifact, err := findArtifact(ctx, c, owner, repo, reshareRun.GetID())
	if err != nil {
		t.Fatalf("find reshare artifact: %v", err)
	}
	if reshareArtifact.GetID() == artifact.GetID() {
		t.Fatal("reshare produced the same artifact as the original share")
	}
	if body := downloadArtifact(ctx, t, c, owner, repo, reshareArtifact.GetID()); string(body) != "<h1>E2E</h1>" {
		t.Fatalf("reshared artifact content = %q, want %q", body, "<h1>E2E</h1>")
	}

	// The reshare records itself too, which is what lets its own artifact URL be
	// reshared in turn.
	reshareRecord := artifactRecord{
		ArtifactURL:  artifactURL(owner, repo, reshareRun.GetID(), reshareArtifact.GetID()),
		ArtifactID:   reshareArtifact.GetID(),
		RunID:        reshareRun.GetID(),
		Commit:       reshareSHA,
		PayloadDir:   stored.PayloadDir,
		Input:        stored.Input,
		InputType:    stored.InputType,
		ResharedFrom: artifact.GetID(),
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if err := recordArtifact(ctx, c, owner, repo, branch, reshareRecord); err != nil {
		t.Fatalf("record reshared artifact: %v", err)
	}
	chained, err := readArtifactRecord(ctx, c, owner, repo, branch, reshareArtifact.GetID())
	if err != nil {
		t.Fatalf("read reshared artifact record: %v", err)
	}
	if chained.PayloadDir != record.PayloadDir {
		t.Errorf("reshared record payload dir = %q, want the original %q", chained.PayloadDir, record.PayloadDir)
	}
	if chained.ResharedFrom != artifact.GetID() {
		t.Errorf("reshared record reshared_from = %d, want %d", chained.ResharedFrom, artifact.GetID())
	}

	// One payload directory regardless of how many times it was shared.
	_, payloads, _, err := c.Repositories.GetContents(ctx, owner, repo, payloadsDir, &github.RepositoryContentGetOptions{Ref: branch})
	if err != nil {
		t.Fatalf("list payload directories: %v", err)
	}
	if len(payloads) != 1 {
		t.Fatalf("%s holds %d entries, want 1 after a reshare", payloadsDir, len(payloads))
	}
}

func TestGitHubAPIEndToEndDirectory(t *testing.T) {
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

	branch := fmt.Sprintf("gh-share-e2e-dir-%d", time.Now().UnixNano())
	previousBranch := shareBranch
	shareBranch = branch
	t.Cleanup(func() {
		shareBranch = previousBranch
		_, _ = c.Git.DeleteRef(context.Background(), owner, repo, "heads/"+branch)
	})

	input := filepath.Join(t.TempDir(), "site")
	if err := os.Mkdir(input, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"index.html": "<h1>E2E</h1>",
		".nojekyll":  "",
	} {
		if err := os.WriteFile(filepath.Join(input, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ts := time.Now().UTC().Format("20060102-150405")
	files, err := payloadFiles(input, true, ts)
	if err != nil {
		t.Fatalf("create payload: %v", err)
	}
	files[payloadRefPath] = payloadRef(shareID(), ts, "dir", "site")

	sha, err := commitPayload(ctx, c, owner, repo, branch, "Share payload", files, func(string) {})
	if err != nil {
		t.Fatalf("commit payload: %v", err)
	}

	run, err := waitForRun(ctx, c, owner, repo, sha, func(string) {})
	if err != nil {
		t.Fatalf("wait for workflow: %v", err)
	}
	artifact, err := findArtifact(ctx, c, owner, repo, run.GetID())
	if err != nil {
		t.Fatalf("find artifact: %v", err)
	}

	// A directory artifact is zipped. Payloads live under a hidden directory, so
	// this is where a regression in hidden-file handling shows up: .nojekyll is
	// dropped from the archive rather than failing the upload.
	body := downloadArtifact(ctx, t, c, owner, repo, artifact.GetID())
	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("open artifact archive (%d bytes): %v", len(body), err)
	}
	entries := map[string]string{}
	for _, f := range archive.File {
		r, err := f.Open()
		if err != nil {
			t.Fatalf("open %s in artifact: %v", f.Name, err)
		}
		content, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatalf("read %s in artifact: %v", f.Name, err)
		}
		entries[f.Name] = string(content)
	}
	if got, ok := entries["index.html"]; !ok || got != "<h1>E2E</h1>" {
		t.Fatalf("artifact entries = %#v, want index.html with the payload", entries)
	}
	if _, ok := entries[".nojekyll"]; !ok {
		t.Fatalf("artifact entries = %#v, want .nojekyll to survive the upload", entries)
	}
}

func downloadArtifact(ctx context.Context, t *testing.T, c *github.Client, owner, repo string, artifactID int64) []byte {
	t.Helper()

	u, _, err := c.Actions.DownloadArtifact(ctx, owner, repo, artifactID, 10)
	if err != nil {
		t.Fatalf("resolve artifact download URL: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		t.Fatalf("build artifact download request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("download artifact: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("download artifact: status %d", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	return body
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
	if !strings.Contains(workflow, "read -r SHARE_ID PAYLOAD_DIR PAYLOAD_TYPE PAYLOAD_NAME < "+payloadRefPath) {
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

func TestPayloadRef(t *testing.T) {
	t.Parallel()

	got := string(payloadRef("20260828-120000-4a2f7d6bf6698163", "20260101-090000", "file", "report.html"))
	want := "20260828-120000-4a2f7d6bf6698163 20260101-090000 file report.html\n"
	if got != want {
		t.Errorf("payloadRef() = %q, want %q", got, want)
	}

	// The share ID is the only thing that makes a reshare of an unchanged payload
	// modify the file the workflow's push paths filter watches.
	other := string(payloadRef("20260828-120000-9c1e05b3a7f2d846", "20260101-090000", "file", "report.html"))
	if got == other {
		t.Errorf("payloadRef() is identical for two share IDs: %q", got)
	}
}

func TestPayloadRefKeepsNamesWithSpaces(t *testing.T) {
	t.Parallel()

	// The workflow reads the line with `read -r`, so the name must stay last for
	// the shell to hand it over whole.
	got := string(payloadRef("20260828-120000-4a2f7d6bf6698163", "20260101-090000", "file", "my report.html"))
	if !strings.HasSuffix(got, " my report.html\n") {
		t.Errorf("payloadRef() = %q, want the name to end the line", got)
	}
}

func TestShareIDIsUniquePerCall(t *testing.T) {
	t.Parallel()

	// Timestamps alone collide here, because the clock resolution is only
	// microseconds on some platforms, and a colliding share ID leaves a reshare
	// waiting out its timeout on a workflow run that never starts.
	seen := map[string]struct{}{}
	for range 100 {
		id := shareID()
		if _, ok := seen[id]; ok {
			t.Fatalf("shareID() returned %q twice", id)
		}
		seen[id] = struct{}{}
	}
}

func TestParseArtifactRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ref     string
		want    int64
		wantErr bool
	}{
		{name: "artifact URL", ref: "https://github.com/o/r/actions/runs/123/artifacts/456", want: 456},
		{name: "artifact URL with trailing slash", ref: "https://github.com/o/r/actions/runs/123/artifacts/456/", want: 456},
		{name: "bare ID", ref: "456", want: 456},
		{name: "surrounding whitespace", ref: "  456\n", want: 456},
		{name: "not a number", ref: "not-a-url", wantErr: true},
		{name: "empty", ref: "", wantErr: true},
		{name: "zero", ref: "0", wantErr: true},
		{name: "negative", ref: "-1", wantErr: true},
		// A run URL ends in a run ID, which names a different resource. Resolving
		// it would report the run ID back as an artifact with no record.
		{name: "run URL", ref: "https://github.com/o/r/actions/runs/123", wantErr: true},
		{name: "API artifact URL", ref: "https://api.github.com/repos/o/r/actions/artifacts/456", want: 456},
		{name: "artifacts path without an ID", ref: "https://github.com/o/r/actions/runs/123/artifacts", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseArtifactRef(tt.ref)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseArtifactRef(%q) = %d, want an error", tt.ref, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArtifactRef(%q): %v", tt.ref, err)
			}
			if got != tt.want {
				t.Errorf("parseArtifactRef(%q) = %d, want %d", tt.ref, got, tt.want)
			}
		})
	}
}

func TestArtifactRecordOmitsResharedFromOnFirstShare(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(artifactRecord{ArtifactID: 456, PayloadDir: payloadsDir + "/20260101-090000"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "reshared_from") {
		t.Errorf("artifactRecord = %s, want no reshared_from for a first share", data)
	}
	data, err = json.Marshal(artifactRecord{ArtifactID: 789, ResharedFrom: 456})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"reshared_from": 456`) && !strings.Contains(string(data), `"reshared_from":456`) {
		t.Errorf("artifactRecord = %s, want reshared_from 456", data)
	}
}
