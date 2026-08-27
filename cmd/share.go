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

	"github.com/google/go-github/v79/github"
	"github.com/k1LoW/go-github-client/v79/factory"
	"github.com/spf13/cobra"
)

const defaultBranch = "gh-share-staging"

var shareRepo, shareBranch string
var shareOpen, sharePersist bool

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
	sha, err := commitPayload(ctx, c, owner, repo, shareBranch, files)
	if err != nil {
		return err
	}
	run, err := waitForRun(ctx, c, owner, repo, sha)
	if err != nil {
		return err
	}
	url, err := artifactURL(ctx, c, owner, repo, run.GetID())
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, url)
	if shareOpen {
		if err := openURL(url); err != nil {
			return fmt.Errorf("open artifact URL: %w", err)
		}
	}
	if !keep {
		if _, err := c.Git.DeleteRef(ctx, owner, repo, "heads/"+shareBranch); err != nil {
			return fmt.Errorf("delete staging branch: %w", err)
		}
	}
	return nil
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
	err := filepath.WalkDir(filepath.Clean(input), func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
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

func commitPayload(ctx context.Context, c *github.Client, owner, repo, branch string, files map[string][]byte) (string, error) {
	ref, _, err := c.Git.GetRef(ctx, owner, repo, "heads/"+branch)
	if err != nil {
		var e *github.ErrorResponse
		if !errors.As(err, &e) || e.Response.StatusCode != 404 {
			return "", fmt.Errorf("get staging branch: %w", err)
		}
		return createOrphanCommit(ctx, c, owner, repo, branch, files)
	}
	parent := ref.GetObject().GetSHA()
	baseCommit, _, err := c.Git.GetCommit(ctx, owner, repo, parent)
	if err != nil {
		return "", err
	}
	tree, err := createTree(ctx, c, owner, repo, baseCommit.GetTree().GetSHA(), files)
	if err != nil {
		return "", err
	}
	commit, _, err := c.Git.CreateCommit(ctx, owner, repo, github.Commit{Message: github.String("Share payload"), Tree: tree, Parents: []*github.Commit{{SHA: github.String(parent)}}}, nil)
	if err != nil {
		return "", fmt.Errorf("create commit: %w", err)
	}
	if _, _, err = c.Git.UpdateRef(ctx, owner, repo, "heads/"+branch, github.UpdateRef{SHA: commit.GetSHA()}); err != nil {
		return "", fmt.Errorf("update staging branch: %w", err)
	}
	return commit.GetSHA(), nil
}

func createOrphanCommit(ctx context.Context, c *github.Client, owner, repo, branch string, files map[string][]byte) (string, error) {
	files[".github/workflows/upload-artifact.yml"] = uploadWorkflow
	// GitHub's REST API rejects an empty base_tree for repositories whose tree
	// has not been created through this API yet. Use the default branch tree as
	// the tree base, while leaving the new commit parentless to preserve the
	// orphan-branch design.
	repository, _, err := c.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return "", fmt.Errorf("get repository: %w", err)
	}
	ref, _, err := c.Git.GetRef(ctx, owner, repo, "heads/"+repository.GetDefaultBranch())
	if err != nil {
		return "", fmt.Errorf("get default branch: %w", err)
	}
	baseCommit, _, err := c.Git.GetCommit(ctx, owner, repo, ref.GetObject().GetSHA())
	if err != nil {
		return "", fmt.Errorf("get default branch commit: %w", err)
	}
	tree, err := createTree(ctx, c, owner, repo, baseCommit.GetTree().GetSHA(), files)
	if err != nil {
		return "", err
	}
	commit, _, err := c.Git.CreateCommit(ctx, owner, repo, github.Commit{Message: github.String("Initialize gh-share staging branch"), Tree: tree}, nil)
	if err != nil {
		return "", err
	}
	if _, _, err = c.Git.CreateRef(ctx, owner, repo, github.CreateRef{Ref: "refs/heads/" + branch, SHA: commit.GetSHA()}); err != nil {
		return "", fmt.Errorf("create staging branch: %w", err)
	}
	return commit.GetSHA(), nil
}

func createTree(ctx context.Context, c *github.Client, owner, repo, base string, files map[string][]byte) (*github.Tree, error) {
	entries := make([]*github.TreeEntry, 0, len(files))
	var err error
	for path, data := range files {
		blob, _, err := c.Git.CreateBlob(ctx, owner, repo, github.Blob{Content: github.String(base64.StdEncoding.EncodeToString(data)), Encoding: github.String("base64")})
		if err != nil {
			return nil, fmt.Errorf("create blob %s: %w", path, err)
		}
		if blob.GetSHA() == "" {
			return nil, fmt.Errorf("create blob %s: GitHub returned no blob SHA", path)
		}
		entries = append(entries, &github.TreeEntry{Path: github.String(path), Mode: github.String("100644"), Type: github.String("blob"), SHA: blob.SHA})
	}
	var tree *github.Tree
	for attempt := 0; attempt < 5; attempt++ {
		var response *github.Response
		tree, response, err = c.Git.CreateTree(ctx, owner, repo, base, entries)
		if err == nil {
			return tree, nil
		}
		var apiErr *github.ErrorResponse
		if !errors.As(err, &apiErr) || response == nil || response.StatusCode != 404 || attempt == 4 {
			return nil, fmt.Errorf("create tree (base %q, entries %d): %w", base, len(entries), err)
		}
		delay := time.Duration(1<<attempt) * time.Second
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, errors.New("create tree: unreachable retry state")
}

func waitForRun(ctx context.Context, c *github.Client, owner, repo, sha string) (*github.WorkflowRun, error) {
	deadline := time.NewTimer(15 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		runs, _, err := c.Actions.ListWorkflowRunsByFileName(ctx, owner, repo, "upload-artifact.yml", &github.ListWorkflowRunsOptions{Branch: shareBranch, ListOptions: github.ListOptions{PerPage: 10}})
		if err != nil {
			return nil, err
		}
		for _, run := range runs.WorkflowRuns {
			if run.GetHeadSHA() == sha {
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
