package cmd

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/briandowns/spinner"
	"github.com/fatih/color"
	"github.com/google/go-github/v79/github"
	"github.com/k1LoW/gh-share/version"
	"github.com/k1LoW/go-github-client/v79/factory"
	"github.com/mattn/go-colorable"
	"github.com/spf13/cobra"
)

const (
	defaultBranch = "gh-share-staging"
	workflowFile  = "upload-gh-share-payload.yml"

	metaDir        = ".gh-share"
	payloadRefPath = metaDir + "/payload-ref"
	payloadsDir    = metaDir + "/payloads"
	artifactsDir   = metaDir + "/artifacts"
	persistPath    = metaDir + "/persist"
	workflowPath   = ".github/workflows/" + workflowFile

	// Staging branches created before the .gh-share/ layout carry the marker at
	// the repository root. Reading it keeps those branches from being treated as
	// unmarked and deleted on the next share or purge.
	legacyPersistPath = ".gh-share-persist"
)

var shareRepo, shareBranch string
var shareOpen, sharePersist, shareJSON, sharePurge bool

var rootCmd = func() *cobra.Command {
	cmd := newShareCommand()
	cmd.Version = version.Version
	return cmd
}()

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newShareCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "share <file|dir>",
		Short: "Share a single HTML file or other files and directories through GitHub Actions artifacts",
		Long: `Share a single HTML file or other files and directories using GitHub's built-in Git and Actions APIs.

gh-share creates a temporary staging branch, commits the payload and an upload workflow, waits for GitHub Actions to upload the artifact, and removes the branch when finished.

Use --purge without an input path to delete gh-share workflow runs, their artifacts and logs, and associated staging branches.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if sharePurge {
				return cobra.NoArgs(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if sharePurge {
				return purge(cmd.Context(), cmd.InOrStdin(), cmd.ErrOrStderr())
			}
			return share(cmd.Context(), args[0])
		},
	}
	cmd.Flags().StringVar(&shareRepo, "repo", "", "Target repository (owner/repo; defaults to the current repository)")
	cmd.Flags().StringVar(&shareBranch, "branch", defaultBranch, "Staging branch name")
	cmd.Flags().BoolVar(&shareOpen, "open", false, "Open the artifact URL in the browser")
	cmd.Flags().BoolVar(&sharePersist, "persist", false, "Keep the staging branch after upload")
	cmd.Flags().BoolVar(&shareJSON, "json", false, "Output upload details as JSON")
	cmd.Flags().BoolVar(&sharePurge, "purge", false, "Delete gh-share workflow runs, artifacts, and staging branches")
	return cmd
}

func purge(ctx context.Context, in io.Reader, out io.Writer) error {
	owner, repo, err := repository(ctx, shareRepo)
	if err != nil {
		return err
	}
	c, err := factory.NewGithubClient()
	if err != nil {
		return fmt.Errorf("create GitHub client: %w", err)
	}

	s := spinner.New(spinner.CharSets[11], 100*time.Millisecond, spinner.WithWriter(colorable.NewColorableStderr()))
	_ = s.Color("fgCyan")
	s.Suffix = " Finding gh-share workflow runs"
	s.Start()
	var runs []*github.WorkflowRun
	for page := 1; ; page++ {
		s.Suffix = fmt.Sprintf(" Finding gh-share workflow runs (page %d)", page)
		list, response, err := c.Actions.ListWorkflowRunsByFileName(ctx, owner, repo, workflowFile, &github.ListWorkflowRunsOptions{
			ListOptions: github.ListOptions{Page: page, PerPage: 100},
		})
		if err != nil {
			s.Stop()
			var apiErr *github.ErrorResponse
			if errors.As(err, &apiErr) && apiErr.Response.StatusCode == 404 {
				break
			}
			return fmt.Errorf("list gh-share workflow runs: %w", err)
		}
		runs = append(runs, list.WorkflowRuns...)
		if response == nil || response.NextPage == 0 {
			break
		}
	}
	s.Stop()

	for _, run := range runs {
		if run.GetStatus() != "completed" {
			return fmt.Errorf("workflow run %d is still %s; cancel it and retry", run.GetID(), run.GetStatus())
		}
	}

	branches := map[string]struct{}{}
	for _, run := range runs {
		if branch := run.GetHeadBranch(); branch != "" {
			branches[branch] = struct{}{}
		}
	}
	if len(runs) == 0 {
		if shareJSON {
			return json.NewEncoder(os.Stdout).Encode(struct {
				WorkflowRuns    int `json:"workflow_runs_deleted"`
				StagingBranches int `json:"staging_branches_deleted"`
			}{})
		}
		fmt.Fprintf(out, "No gh-share workflow runs found in %s/%s.\n", owner, repo)
		return nil
	}

	if ok, err := confirmPurge(in, out, owner, repo, len(runs), len(branches)); err != nil {
		return err
	} else if !ok {
		if shareJSON {
			return json.NewEncoder(os.Stdout).Encode(struct {
				Purged bool `json:"purged"`
			}{false})
		}
		return nil
	}
	s = spinner.New(spinner.CharSets[11], 100*time.Millisecond, spinner.WithWriter(colorable.NewColorableStderr()))
	_ = s.Color("fgCyan")
	s.Start()
	defer s.Stop()
	for i, run := range runs {
		s.Suffix = fmt.Sprintf(" Deleting workflow runs (%d/%d)", i+1, len(runs))
		if _, err := c.Actions.DeleteWorkflowRun(ctx, owner, repo, run.GetID()); err != nil {
			return fmt.Errorf("delete workflow run %d: %w", run.GetID(), err)
		}
	}

	repositoryInfo, _, err := c.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("get repository: %w", err)
	}
	deletedBranches := 0
	processedBranches := 0
	for branch := range branches {
		processedBranches++
		s.Suffix = fmt.Sprintf(" Checking staging branches (%d/%d)", processedBranches, len(branches))
		if branch == repositoryInfo.GetDefaultBranch() {
			continue
		}
		persist, err := hasPersistMarker(ctx, c, owner, repo, branch)
		if err != nil {
			return fmt.Errorf("check persist marker for staging branch %s: %w", branch, err)
		}
		if persist {
			continue
		}
		if _, err := c.Git.DeleteRef(ctx, owner, repo, "heads/"+branch); err != nil {
			var apiErr *github.ErrorResponse
			if errors.As(err, &apiErr) && (apiErr.Response.StatusCode == 404 || apiErr.Message == "Reference does not exist") {
				continue
			}
			return fmt.Errorf("delete staging branch %s: %w", branch, err)
		}
		deletedBranches++
	}
	s.FinalMSG = fmt.Sprintf("\nPurged %d gh-share workflow run(s) and %d staging branch(es) from %s/%s.\n", len(runs), deletedBranches, owner, repo)

	if shareJSON {
		result := struct {
			WorkflowRuns    int `json:"workflow_runs_deleted"`
			StagingBranches int `json:"staging_branches_deleted"`
		}{len(runs), deletedBranches}
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	return nil
}

func confirmPurge(in io.Reader, out io.Writer, owner, repo string, workflowRuns, branches int) (bool, error) {
	fmt.Fprintf(out, "Purge %d gh-share workflow run(s) and up to %d staging branch(es) from %s/%s? [y/N] ", workflowRuns, branches, owner, repo)
	answer, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read purge confirmation: %w", err)
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer != "y" && answer != "yes" {
		fmt.Fprintln(out, "Purge canceled.")
		return false, nil
	}
	return true, nil
}

func share(ctx context.Context, input string) error {
	info, err := os.Stat(input)
	if err != nil {
		return fmt.Errorf("stat input: %w", err)
	}
	c, owner, repo, err := prepare(ctx)
	if err != nil {
		return err
	}
	marker, err := hasPersistMarker(ctx, c, owner, repo, shareBranch)
	if err != nil {
		return err
	}
	ts := time.Now().UTC().Format("20060102-150405")
	files, err := payloadFiles(input, info.IsDir(), ts)
	if err != nil {
		return err
	}
	kind := "file"
	if info.IsDir() {
		kind = "dir"
	}
	name := filepath.Base(filepath.Clean(input))
	files[payloadRefPath] = []byte(ts + " " + kind + " " + name + "\n")
	if sharePersist {
		// Written unconditionally so a branch still marked by the pre-.gh-share/
		// path picks up the current one. Rewriting identical content reuses the
		// existing blob, so the commit carries no change for this path.
		files[persistPath] = []byte("\n")
	}
	return upload(ctx, c, owner, repo, payloadPlan{
		files:      files,
		payloadDir: payloadsDir + "/" + ts,
		name:       name,
		kind:       kind,
		keep:       marker || sharePersist,
	})
}

func prepare(ctx context.Context) (*github.Client, string, string, error) {
	if shareBranch == "" || strings.ContainsAny(shareBranch, " ~^:?*[\\\\") {
		return nil, "", "", errors.New("invalid branch name")
	}
	owner, repo, err := repository(ctx, shareRepo)
	if err != nil {
		return nil, "", "", err
	}
	c, err := factory.NewGithubClient()
	if err != nil {
		return nil, "", "", fmt.Errorf("create GitHub client: %w", err)
	}
	return c, owner, repo, nil
}

// payloadPlan is what a share commits to the staging branch and what the result
// describes, so that uploading does not depend on how the payload was assembled.
type payloadPlan struct {
	files      map[string][]byte
	payloadDir string
	name       string
	kind       string
	keep       bool
}

func upload(ctx context.Context, c *github.Client, owner, repo string, plan payloadPlan) error {
	target := fmt.Sprintf("%s/%s@%s", owner, repo, shareBranch)

	s := spinner.New(spinner.CharSets[11], 100*time.Millisecond, spinner.WithWriter(colorable.NewColorableStderr()))
	_ = s.Color("fgCyan")
	s.Suffix = " Preparing branch: " + target
	s.Start()
	defer s.Stop()

	sha, err := commitPayload(ctx, c, owner, repo, shareBranch, "Share payload", plan.files, func(message string) {
		s.Suffix = " " + message + " (" + target + ")"
	})
	if err != nil {
		return err
	}
	commitURL := fmt.Sprintf("https://github.com/%s/%s/commit/%s", owner, repo, sha)
	s.Suffix = " Starting workflow: " + commitURL
	run, err := waitForRun(ctx, c, owner, repo, sha, func(message string) {
		s.Suffix = " " + message
	})
	if err != nil {
		return err
	}
	runURL := fmt.Sprintf("https://github.com/%s/%s/actions/runs/%d", owner, repo, run.GetID())
	s.Suffix = " Workflow completed: " + runURL

	s.Suffix = " Checking artifact: " + runURL
	artifact, err := findArtifact(ctx, c, owner, repo, run.GetID())
	if err != nil {
		return err
	}
	url := artifactURL(owner, repo, run.GetID(), artifact.GetID())
	if plan.keep {
		s.Suffix = " Recording artifact: " + target
		record := artifactRecord{
			ArtifactURL: url,
			ArtifactID:  artifact.GetID(),
			RunID:       run.GetID(),
			Commit:      sha,
			PayloadDir:  plan.payloadDir,
			Input:       plan.name,
			InputType:   plan.kind,
			CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		}
		if err := recordArtifact(ctx, c, owner, repo, shareBranch, record); err != nil {
			// The upload already succeeded, so the URL must survive the error or
			// the user loses the only thing this command exists to produce.
			return fmt.Errorf("%w (artifact URL: %s)", err, url)
		}
	}
	if shareOpen {
		if err := openURL(url); err != nil {
			return fmt.Errorf("open artifact URL: %w", err)
		}
	}
	if !plan.keep {
		s.Suffix = " Deleting branch: " + target
		if _, err := c.Git.DeleteRef(ctx, owner, repo, "heads/"+shareBranch); err != nil {
			return fmt.Errorf("delete staging branch: %w", err)
		}
	} else {
		s.Suffix = " Keeping branch: " + target
	}
	branchStatus := "deleted"
	if plan.keep {
		branchStatus = "kept"
	}
	branchURL := fmt.Sprintf("https://github.com/%s/%s/tree/%s", owner, repo, shareBranch)
	uploadMessage := fmt.Sprintf("Successfully uploaded %s.", plan.name)
	if plan.kind == "dir" {
		uploadMessage = fmt.Sprintf("Successfully uploaded directory: %s", plan.name)
	}
	result := shareResult{
		Input:         plan.name,
		InputType:     plan.kind,
		Repository:    fmt.Sprintf("https://github.com/%s/%s", owner, repo),
		Branch:        branchURL,
		BranchDeleted: !plan.keep,
		Commit:        commitURL,
		Workflow:      runURL,
		Artifact:      url,
	}
	if !shareJSON {
		s.FinalMSG = "\n" + uploadMessage + "\n\n" + formatSummary(branchURL, branchStatus, commitURL, runURL) + formatArtifactURL(url)
	}
	s.Stop()
	if shareJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(result); err != nil {
			return fmt.Errorf("encode JSON output: %w", err)
		}
	}
	return nil
}

type shareResult struct {
	Input         string `json:"input"`
	InputType     string `json:"input_type"`
	Repository    string `json:"repository"`
	Branch        string `json:"branch"`
	BranchDeleted bool   `json:"branch_deleted"`
	Commit        string `json:"commit"`
	Workflow      string `json:"workflow"`
	Artifact      string `json:"artifact"`
}

func formatSummary(branchURL, branchStatus, commitURL, runURL string) string {
	rows := []struct {
		label string
		value string
	}{
		{label: "Branch:   ", value: fmt.Sprintf("%s (%s)", branchURL, branchStatus)},
		{label: "Commit:   ", value: commitURL},
		{label: "Workflow: ", value: runURL},
	}
	width := 0
	for _, row := range rows {
		if length := len(row.label) + len(row.value); length > width {
			width = length
		}
	}

	var summary strings.Builder
	borderStyle := color.New(color.FgCyan)
	labelStyle := color.New(color.FgCyan, color.Bold)
	valueStyle := color.New(color.FgWhite)
	topBorder := "╔" + strings.Repeat("═", width+2) + "╗\n"
	bottomBorder := "╚" + strings.Repeat("═", width+2) + "╝\n"
	summary.WriteString(borderStyle.Sprint(topBorder))
	for _, row := range rows {
		padding := strings.Repeat(" ", width-len(row.label)-len(row.value))
		fmt.Fprintf(&summary, "%s %s%s%s %s\n", borderStyle.Sprint("║"), labelStyle.Sprint(row.label), valueStyle.Sprint(row.value), padding, borderStyle.Sprint("║"))
	}
	summary.WriteString(borderStyle.Sprint(bottomBorder))
	summary.WriteByte('\n')
	return summary.String()
}

func formatArtifactURL(url string) string {
	labelStyle := color.New(color.FgCyan, color.Bold)
	valueStyle := color.New(color.FgWhite)
	return fmt.Sprintf("%s\n%s\n\n", labelStyle.Sprint("Artifact URL:"), valueStyle.Sprint(url))
}

func repository(ctx context.Context, selector string) (string, string, error) {
	args := []string{"repo", "view"}
	if selector != "" {
		args = append(args, selector)
	}
	args = append(args, "--json", "owner,name")
	command := exec.CommandContext(ctx, "gh", args...)
	out, err := command.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", "", fmt.Errorf("resolve repository with gh: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", "", fmt.Errorf("resolve repository with gh: %w", err)
	}
	var v struct {
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &v); err != nil || v.Owner.Login == "" || v.Name == "" {
		return "", "", errors.New("could not determine repository owner/name")
	}
	return v.Owner.Login, v.Name, nil
}

func payloadFiles(input string, dir bool, ts string) (map[string][]byte, error) {
	files := map[string][]byte{}
	if !dir {
		data, err := os.ReadFile(input)
		if err != nil {
			return nil, fmt.Errorf("read input: %w", err)
		}
		files[filepath.ToSlash(filepath.Join(payloadsDir, ts, filepath.Base(input)))] = data
		return files, nil
	}
	rootDir, err := os.OpenRoot(filepath.Clean(input))
	if err != nil {
		return nil, fmt.Errorf("open input directory: %w", err)
	}
	defer rootDir.Close()
	err = fs.WalkDir(rootDir.FS(), ".", func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		data, err := rootDir.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(filepath.Join(payloadsDir, ts, path))] = data
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read directory: %w", err)
	}
	if len(files) == 0 {
		return nil, errors.New("directory is empty")
	}
	return files, nil
}

func hasPersistMarker(ctx context.Context, c *github.Client, owner, repo, branch string) (bool, error) {
	for _, path := range []string{persistPath, legacyPersistPath} {
		found, err := branchContains(ctx, c, owner, repo, branch, path)
		if err != nil {
			return false, fmt.Errorf("check persist marker: %w", err)
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

func branchContains(ctx context.Context, c *github.Client, owner, repo, branch, path string) (bool, error) {
	_, _, _, err := c.Repositories.GetContents(ctx, owner, repo, path, &github.RepositoryContentGetOptions{Ref: branch})
	if err == nil {
		return true, nil
	}
	var e *github.ErrorResponse
	if errors.As(err, &e) && (e.Response.StatusCode == 404 || e.Response.StatusCode == 409) {
		return false, nil
	}
	return false, err
}

func commitPayload(ctx context.Context, c *github.Client, owner, repo, branch, message string, files map[string][]byte, progress func(string)) (string, error) {
	ref, _, err := c.Git.GetRef(ctx, owner, repo, "heads/"+branch)
	if err != nil {
		var e *github.ErrorResponse
		if !errors.As(err, &e) || e.Response.StatusCode != 404 {
			return "", fmt.Errorf("get staging branch: %w", err)
		}
		repository, _, err := c.Repositories.Get(ctx, owner, repo)
		if err != nil {
			return "", fmt.Errorf("get repository: %w", err)
		}
		base, _, err := c.Git.GetRef(ctx, owner, repo, "heads/"+repository.GetDefaultBranch())
		if err != nil {
			return "", fmt.Errorf("get default branch: %w", err)
		}
		progress("Creating branch")
		if _, _, err = c.Git.CreateRef(ctx, owner, repo, github.CreateRef{Ref: "refs/heads/" + branch, SHA: base.GetObject().GetSHA()}); err != nil {
			return "", fmt.Errorf("create staging branch: %w", err)
		}
		ref = base
	}
	baseCommit, _, err := c.Git.GetCommit(ctx, owner, repo, ref.GetObject().GetSHA())
	if err != nil {
		return "", fmt.Errorf("get staging commit: %w", err)
	}
	files[workflowPath] = uploadWorkflow
	progress("Committing")
	tree, err := createTree(ctx, c, owner, repo, ref.GetObject().GetSHA(), files)
	if err != nil {
		return "", err
	}
	commit, _, err := c.Git.CreateCommit(ctx, owner, repo, github.Commit{Message: new(message), Tree: tree, Parents: []*github.Commit{baseCommit}}, nil)
	if err != nil {
		return "", fmt.Errorf("create commit: %w", err)
	}
	if _, _, err = c.Git.UpdateRef(ctx, owner, repo, "heads/"+branch, github.UpdateRef{SHA: commit.GetSHA(), Force: new(true)}); err != nil {
		return "", fmt.Errorf("update staging branch: %w", err)
	}
	progress(fmt.Sprintf("Committed: https://github.com/%s/%s/commit/%s", owner, repo, commit.GetSHA()))
	return commit.GetSHA(), nil
}

func createTree(ctx context.Context, c *github.Client, owner, repo, base string, files map[string][]byte) (*github.Tree, error) {
	entries := make(map[string]*github.TreeEntry, len(files))
	for path, data := range files {
		blob, _, err := c.Git.CreateBlob(ctx, owner, repo, github.Blob{Content: new(base64.StdEncoding.EncodeToString(data)), Encoding: new("base64")})
		if err != nil {
			return nil, fmt.Errorf("create blob %s: %w", path, err)
		}
		if blob.GetSHA() == "" {
			return nil, fmt.Errorf("create blob %s: GitHub returned no blob SHA", path)
		}
		entries[path] = &github.TreeEntry{Path: new(path), Mode: new("100644"), Type: new("blob"), SHA: blob.SHA}
	}
	return buildTree(ctx, c, owner, repo, base, entries)
}

func buildTree(ctx context.Context, c *github.Client, owner, repo, base string, files map[string]*github.TreeEntry) (*github.Tree, error) {
	baseEntries := map[string]*github.TreeEntry{}
	if base != "" {
		old, _, err := c.Git.GetTree(ctx, owner, repo, base, false)
		if err != nil {
			return nil, fmt.Errorf("get base tree: %w", err)
		}
		for i := range old.Entries {
			baseEntries[old.Entries[i].GetPath()] = old.Entries[i]
		}
	}
	children := map[string]map[string]*github.TreeEntry{}
	for path, entry := range files {
		parts := strings.SplitN(path, "/", 2)
		if len(parts) == 1 {
			entry.Path = new(parts[0])
			children[parts[0]] = map[string]*github.TreeEntry{"": entry}
			continue
		}
		if children[parts[0]] == nil {
			children[parts[0]] = map[string]*github.TreeEntry{}
		}
		children[parts[0]][strings.TrimPrefix(path, parts[0]+"/")] = entry
	}
	entries := make([]*github.TreeEntry, 0, len(children))
	for name, group := range children {
		if direct, ok := group[""]; ok {
			entries = append(entries, direct)
			continue
		}
		childBase := ""
		if old := baseEntries[name]; old != nil && old.GetType() == "tree" {
			childBase = old.GetSHA()
		}
		child, err := buildTree(ctx, c, owner, repo, childBase, group)
		if err != nil {
			return nil, err
		}
		entries = append(entries, &github.TreeEntry{Path: new(name), Mode: new("040000"), Type: new("tree"), SHA: child.SHA})
	}
	tree, _, err := c.Git.CreateTree(ctx, owner, repo, base, entries)
	if err != nil {
		return nil, fmt.Errorf("create tree (base %q, entries %d): %w", base, len(entries), err)
	}
	return tree, nil
}

func waitForRun(ctx context.Context, c *github.Client, owner, repo, sha string, progress func(string)) (*github.WorkflowRun, error) {
	deadline := time.NewTimer(15 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		runs, response, err := c.Actions.ListWorkflowRunsByFileName(ctx, owner, repo, workflowFile, &github.ListWorkflowRunsOptions{Branch: shareBranch, ListOptions: github.ListOptions{PerPage: 10}})
		if err != nil {
			var apiErr *github.ErrorResponse
			if response == nil || response.StatusCode != 404 || !errors.As(err, &apiErr) {
				return nil, err
			}
		}
		if runs == nil {
			runs = &github.WorkflowRuns{}
		}
		for _, run := range runs.WorkflowRuns {
			if run.GetHeadSHA() == sha {
				progress(fmt.Sprintf("Workflow running: https://github.com/%s/%s/actions/runs/%d", owner, repo, run.GetID()))
				if run.GetStatus() == "completed" {
					if run.GetConclusion() != "success" {
						return nil, fmt.Errorf("workflow failed with conclusion %s", run.GetConclusion())
					}
					return run, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, errors.New("timed out waiting for workflow run")
		case <-ticker.C:
		}
	}
}

func findArtifact(ctx context.Context, c *github.Client, owner, repo string, runID int64) (*github.Artifact, error) {
	list, _, err := c.Actions.ListWorkflowRunArtifacts(ctx, owner, repo, runID, nil)
	if err != nil {
		return nil, err
	}
	if len(list.Artifacts) == 0 {
		return nil, errors.New("workflow completed without an artifact")
	}
	return list.Artifacts[0], nil
}

func artifactURL(owner, repo string, runID, artifactID int64) string {
	return fmt.Sprintf("https://github.com/%s/%s/actions/runs/%d/artifacts/%d", owner, repo, runID, artifactID)
}

type artifactRecord struct {
	ArtifactURL string `json:"artifact_url"`
	ArtifactID  int64  `json:"artifact_id"`
	RunID       int64  `json:"run_id"`
	Commit      string `json:"commit"`
	PayloadDir  string `json:"payload_dir"`
	Input       string `json:"input"`
	InputType   string `json:"input_type"`
	CreatedAt   string `json:"created_at"`
}

func artifactRecordPath(artifactID int64) string {
	return fmt.Sprintf("%s/%d.json", artifactsDir, artifactID)
}

// recordArtifact writes the record outside the workflow trigger path, so the
// commit it creates does not start a second upload run for the same payload.
// Keying the file by artifact ID keeps the lookup a single API call even after
// --purge has removed the workflow run the URL points at.
func recordArtifact(ctx context.Context, c *github.Client, owner, repo, branch string, record artifactRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode artifact record: %w", err)
	}
	files := map[string][]byte{artifactRecordPath(record.ArtifactID): append(data, '\n')}
	if _, err := commitPayload(ctx, c, owner, repo, branch, "Record shared artifact", files, func(string) {}); err != nil {
		return fmt.Errorf("record shared artifact: %w", err)
	}
	return nil
}

func openURL(url string) error {
	command, args := "xdg-open", []string{url}
	switch runtime.GOOS {
	case "darwin":
		command = "open"
	case "windows":
		command, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	}
	return exec.Command(command, args...).Run()
}
