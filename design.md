# gh-share

A GitHub CLI extension that uploads local files to GitHub Actions Artifacts and returns a shareable URL, scoped to repository permissions.

## Motivation

Sharing files (HTML reports, images, PDFs, etc.) within a repository's access control is cumbersome. GitHub Pages requires a branch commit. External services like Netlify or Gist either leak outside repo permissions or do not render HTML. GitHub Actions Artifacts with `actions/upload-artifact@v7` (`archive: false`) support direct browser viewing for HTML, images, and other natively renderable files — without committing to main or PR branches.

## Design Goals

- No commits to main or PR branches
- Repository-scoped access (GitHub authentication required)
- Shareable URL that renders HTML, images, etc. directly in the browser
- Works for any file type (non-renderable files are downloadable)
- Simple one-command UX

## Usage

```
gh share <file|dir> [flags]

Flags:
  --open            Open the artifact URL in the browser after upload
  --repo <repo>     Target repository (default: current repo, format: owner/repo)
  --branch <name>   Staging branch name (default: gh-share-staging)
  --persist         Keep the staging branch after upload; write .gh-share-persist to the branch
```

### Examples

```bash
# Share an HTML file and print the URL
gh share pr123.html

# Share and open in browser
gh share report.html --open

# Share a directory as a zip
gh share ./dist

# Share against a specific repo
gh share report.pdf --repo owner/repo

# Use a custom branch name and keep it alive
gh share report.html --branch my-previews --persist
```

## Architecture

### Key Insight

GitHub Actions workflow files on a branch are executed when that branch receives a push event — even if the branch is not the default branch. The extension creates a temporary staging branch from the default branch, then commits the workflow and payload without touching the default branch.

### Branch Strategy

A staging branch (default: `gh-share-staging`, configurable via `--branch`) is used as a staging area. GitHub's REST Git API cannot point a ref at an unreachable parentless commit, so the initial staging commit is based on the default branch:

- The default branch is never modified
- Contains `.github/workflows/upload-gh-share-payload.yml`, `.gh-share-payload-ref`, and timestamped payload directories
- **Default**: branch is deleted after the workflow run completes
- **Persist mode**: branch is kept alive. `.gh-share-persist` exists on the branch as a marker. Subsequent runs that detect `.gh-share-persist` automatically keep the branch without requiring `--persist` again.
- Because each upload lands in its own timestamped directory, all previously uploaded files remain on the branch when in persist mode.
- Deleting the branch does NOT delete artifacts (artifacts are tied to workflow runs, not branches)

### Input Handling

| Input type | Behavior |
|---|---|
| Single file | Placed under `gh-share-payload/<timestamp>/`, uploaded with `archive: false` |
| Directory | Placed under `gh-share-payload/<timestamp>/` as-is, uploaded with `archive: true` (upload-artifact handles zipping) |

### Payload Reference

Each commit writes the current timestamp directory name and input type (`file` or `dir`) to `.gh-share-payload-ref` at the branch root. The workflow reads this to set the correct `archive` value:

```
20260827-143845 file
```

```
gh-share-payload/
  20260827-143845/
    report.html       <- current upload
  20260827-120000/
    screenshot.png    <- previous upload (persist mode only)
.gh-share-payload-ref          <- contains "20260827-143845"
```

### Workflow

```
[local]                         [GitHub]

gh share file.html
  |
  ├─ 1. Ensure staging branch exists
  |       If not: create branch from the default branch
  |               add .github/workflows/upload-gh-share-payload.yml
  |               push
  |       If .gh-share-persist present on branch: treat as persist mode
  |
  ├─ 2. Prepare payload
  |       file  -> place under gh-share-payload/<timestamp>/
  |       dir   -> zip locally -> place under gh-share-payload/<timestamp>/
  |
  ├─ 3. Commit to staging branch
  |       - gh-share-payload/<timestamp>/<file>
  |       - .gh-share-payload-ref (contains "<timestamp>")
  |       - .gh-share-persist (only on first --persist use)
  |
  ├─ 4. Wait for workflow run to complete (gh run watch)
  |
  ├─ 5. Retrieve artifact URL via GitHub API
  |
  ├─ 6. Print URL to stdout
  |
  ├─ 7. (--open) Open URL in browser
  |
  └─ 8. Delete staging branch (skip if .gh-share-persist present on branch)
```

### Workflow File (embedded in binary)

`.github/workflows/upload-gh-share-payload.yml` placed on the staging branch. Its name is `gh-share: Upload Payload`, making it clear that the workflow was created by gh-share. The workflow triggers on pushes to `.gh-share-payload-ref` (path filter) so it works regardless of the branch name chosen via `--branch`. It reads `.gh-share-payload-ref` to find the exact timestamped directory to upload:

```yaml
name: gh-share: Upload Payload

on:
  push:
    paths:
      - '.gh-share-payload-ref'

jobs:
  upload:
    runs-on: ubuntu-slim
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@v4

      - name: Read payload ref
        run: |
          PAYLOAD_DIR=$(awk '{print $1}' .gh-share-payload-ref)
          PAYLOAD_TYPE=$(awk '{print $2}' .gh-share-payload-ref)
          echo "PAYLOAD_DIR=$PAYLOAD_DIR" >> $GITHUB_ENV
          echo "PAYLOAD_TYPE=$PAYLOAD_TYPE" >> $GITHUB_ENV

      - name: Upload (file)
        if: env.PAYLOAD_TYPE == 'file'
        uses: actions/upload-artifact@v7
        with:
          name: ${{ github.sha }}
          path: gh-share-payload/${{ env.PAYLOAD_DIR }}/
          archive: false

      - name: Upload (dir)
        if: env.PAYLOAD_TYPE == 'dir'
        uses: actions/upload-artifact@v7
        with:
          name: ${{ github.sha }}
          path: gh-share-payload/${{ env.PAYLOAD_DIR }}/
          archive: true
```

The file to be shared is placed under `gh-share-payload/` directory on the `gh-share-staging` branch.

### Artifact URL

After the workflow completes, the artifact URL is retrieved via the GitHub Actions REST API:

```
GET /repos/{owner}/{repo}/actions/runs/{run_id}/artifacts
```

The resulting artifact URL is of the form:

```
https://github.com/{owner}/{repo}/actions/runs/{run_id}/artifacts/{artifact_id}
```

This URL requires GitHub login and is scoped to users with repository read access.

## Implementation

### Language

Go, compiled as a `gh` extension binary (`gh-share`).

### Dependencies

- `github.com/k1LoW/go-github-client/v89` — GitHub REST API client with `gh` auth (`factory.NewGithubClient()`)
- `github.com/spf13/cobra` — CLI framework
- Standard library only otherwise

### File Structure

```
gh-share/
  main.go
  cmd/
    root.go         -- cobra root command
    share.go        -- main share logic
  internal/
    branch/
      branch.go     -- staging branch setup and persist detection
    workflow/
      workflow.go   -- embedded workflow YAML, run polling
    artifact/
      artifact.go   -- artifact URL retrieval via API
  embed/
    upload-gh-share-payload.yml -- workflow file embedded via go:embed
```

### Installation

```bash
gh extension install k1LoW/gh-share
```

## Artifact Lifecycle

- Artifacts are retained for **90 days** by default (configurable at repo level)
- Deleting the `gh-share-staging` branch does NOT affect existing artifacts
- Artifacts are deleted only when the associated workflow run is deleted or retention expires

## Limitations

- HTML files with external CSS/JS references may not render correctly (single-file HTML works best)
- Artifact URLs require GitHub login (not publicly accessible without authentication)
- Workflow run startup time adds ~30 seconds of latency per upload
- `archive: false` requires `actions/upload-artifact@v7` or later
