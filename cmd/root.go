package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

const defaultBranch = "gh-share-staging"

var shareRepo, shareBranch string
var shareOpen, sharePersist bool

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
	cmd := &cobra.Command{Use: "share <file|dir>", Short: "Upload a file or directory and print its artifact URL", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error { return share(cmd.Context(), args[0]) }}
	cmd.Flags().StringVar(&shareRepo, "repo", "", "Target repository (owner/repo; defaults to the current repository)")
	cmd.Flags().StringVar(&shareBranch, "branch", defaultBranch, "Staging branch name")
	cmd.Flags().BoolVar(&shareOpen, "open", false, "Open the artifact URL in the browser")
	cmd.Flags().BoolVar(&sharePersist, "persist", false, "Keep the staging branch after upload")
	return cmd
}

func share(ctx context.Context, input string) error {
	info, err := os.Stat(input)
	if err != nil {
		return fmt.Errorf("stat input: %w", err)
	}
	if shareBranch == "" || strings.ContainsAny(shareBranch, " ~^:?*[\\\\") {
		return errors.New("invalid branch name")
	}
	owner, repo, err := repository(ctx, shareRepo)
	if err != nil {
		return err
	}
	c, err := factory.NewGithubClient()
	if err != nil {
		return fmt.Errorf("create GitHub client: %w", err)
	}
	marker, err := hasPersistMarker(ctx, c, owner, repo, shareBranch)
	if err != nil {
		return err
	}
	keep := marker || sharePersist
	target := fmt.Sprintf("%s/%s@%s", owner, repo, shareBranch)
	ts := time.Now().UTC().Format("20060102-150405")
	files, err := payloadFiles(input, info.IsDir(), ts)
	if err != nil {
		return err
	}
	kind := "file"
	if info.IsDir() {
		kind = "dir"
	}
	files[".gh-share-payload-ref"] = []byte(ts + " " + kind + "\n")
	if sharePersist && !marker {
		files[".gh-share-persist"] = []byte("\n")
	}

	s := spinner.New(spinner.CharSets[11], 100*time.Millisecond, spinner.WithWriter(colorable.NewColorableStderr()))
	_ = s.Color("fgCyan")
	s.Suffix = " Preparing branch: " + target
	s.Start()
	defer s.Stop()

	sha, err := commitPayload(ctx, c, owner, repo, shareBranch, files, func(message string) {
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
	url, err := artifactURL(ctx, c, owner, repo, run.GetID())
	if err != nil {
		return err
	}
	if shareOpen {
		if err := openURL(url); err != nil {
			return fmt.Errorf("open artifact URL: %w", err)
		}
	}
	if !keep {
		s.Suffix = " Deleting branch: " + target
		if _, err := c.Git.DeleteRef(ctx, owner, repo, "heads/"+shareBranch); err != nil {
			return fmt.Errorf("delete staging branch: %w", err)
		}
	} else {
		s.Suffix = " Keeping branch: " + target
	}
	branchStatus := "deleted"
	if keep {
		branchStatus = "kept"
	}
	branchURL := fmt.Sprintf("https://github.com/%s/%s/tree/%s", owner, repo, shareBranch)
	inputName := filepath.Base(filepath.Clean(input))
	uploadMessage := fmt.Sprintf("Successfully uploaded %s.", inputName)
	if info.IsDir() {
		uploadMessage = fmt.Sprintf("Successfully uploaded directory: %s", inputName)
	}
	s.FinalMSG = "\n" + uploadMessage + "\n\n" + formatSummary(branchURL, branchStatus, commitURL, runURL) + formatArtifactURL(url)
	s.Stop()
	return nil
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
	args := []string{"repo", "view", "--json", "owner,name"}
	if selector != "" {
		args = append(args, "--repo", selector)
	}
	out, err := exec.CommandContext(ctx, "gh", args...).Output()
	if err != nil {
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
		files[filepath.ToSlash(filepath.Join("gh-share-payload", ts, filepath.Base(input)))] = data
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
		rel, err := filepath.Rel(input, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(filepath.Join("gh-share-payload", ts, rel))] = data
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
	_, _, _, err := c.Repositories.GetContents(ctx, owner, repo, ".gh-share-persist", &github.RepositoryContentGetOptions{Ref: branch})
	if err == nil {
		return true, nil
	}
	var e *github.ErrorResponse
	if errors.As(err, &e) && (e.Response.StatusCode == 404 || e.Response.StatusCode == 409) {
		return false, nil
	}
	return false, fmt.Errorf("check persist marker: %w", err)
}

func commitPayload(ctx context.Context, c *github.Client, owner, repo, branch string, files map[string][]byte, progress func(string)) (string, error) {
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
	files[".github/workflows/upload-gh-share-payload.yml"] = uploadWorkflow
	progress("Committing")
	tree, err := createTree(ctx, c, owner, repo, ref.GetObject().GetSHA(), files)
	if err != nil {
		return "", err
	}
	commit, _, err := c.Git.CreateCommit(ctx, owner, repo, github.Commit{Message: new("Share payload"), Tree: tree, Parents: []*github.Commit{baseCommit}}, nil)
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
		runs, response, err := c.Actions.ListWorkflowRunsByFileName(ctx, owner, repo, "upload-gh-share-payload.yml", &github.ListWorkflowRunsOptions{Branch: shareBranch, ListOptions: github.ListOptions{PerPage: 10}})
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

func artifactURL(ctx context.Context, c *github.Client, owner, repo string, runID int64) (string, error) {
	list, _, err := c.Actions.ListWorkflowRunArtifacts(ctx, owner, repo, runID, nil)
	if err != nil {
		return "", err
	}
	if len(list.Artifacts) == 0 {
		return "", errors.New("workflow completed without an artifact")
	}
	return fmt.Sprintf("https://github.com/%s/%s/actions/runs/%d/artifacts/%d", owner, repo, runID, list.Artifacts[0].GetID()), nil
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
