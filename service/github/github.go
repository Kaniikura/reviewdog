package github

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/go-github/v60/github"

	"github.com/reviewdog/reviewdog"
	"github.com/reviewdog/reviewdog/cienv"
	"github.com/reviewdog/reviewdog/proto/rdf"
	"github.com/reviewdog/reviewdog/service/commentutil"
	"github.com/reviewdog/reviewdog/service/github/githubutils"
	"github.com/reviewdog/reviewdog/service/serviceutil"
)

var _ reviewdog.CommentService = (*PullRequest)(nil)
var _ reviewdog.DiffService = (*PullRequest)(nil)

const maxCommentsPerRequest = 30

const (
	invalidSuggestionPre  = "<details><summary>reviewdog suggestion error</summary>"
	invalidSuggestionPost = "</details>"
)

func isPermissionError(err error) bool {
	var githubErr *github.ErrorResponse
	if !errors.As(err, &githubErr) {
		return false
	}
	status := githubErr.Response.StatusCode
	return status == http.StatusForbidden || status == http.StatusNotFound
}

// PullRequest is a comment and diff service for GitHub PullRequest.
//
// API:
//
//	https://docs.github.com/en/rest/pulls/comments?apiVersion=2022-11-28#create-a-review-comment-for-a-pull-request
//	POST /repos/:owner/:repo/pulls/:number/comments
type PullRequest struct {
	cli   *github.Client
	owner string
	repo  string
	pr    int
	sha   string

	muComments    sync.Mutex
	postComments  []*reviewdog.Comment
	logWriter     *githubutils.GitHubActionLogWriter
	fallbackToLog bool

	postedcs commentutil.PostedComments

	// wd is working directory relative to root of repository.
	wd string
}

// NewGitHubPullRequest returns a new PullRequest service.
// PullRequest service needs git command in $PATH.
//
// The GitHub Token generated by GitHub Actions may not have the necessary permissions.
// For example, in the case of a PR from a forked repository, or when write permission is prohibited in the repository settings [1].
//
// In such a case, the service will fallback to GitHub Actions workflow commands [2].
//
// [1]: https://docs.github.com/en/actions/security-guides/automatic-token-authentication#permissions-for-the-github_token
// [2]: https://docs.github.com/en/actions/reference/workflow-commands-for-github-actions
func NewGitHubPullRequest(cli *github.Client, owner, repo string, pr int, sha, level string) (*PullRequest, error) {
	workDir, err := serviceutil.GitRelWorkdir()
	if err != nil {
		return nil, fmt.Errorf("PullRequest needs 'git' command: %w", err)
	}
	return &PullRequest{
		cli:       cli,
		owner:     owner,
		repo:      repo,
		pr:        pr,
		sha:       sha,
		logWriter: githubutils.NewGitHubActionLogWriter(level),
		wd:        workDir,
	}, nil
}

// Post accepts a comment and holds it. Flush method actually posts comments to
// GitHub in parallel.
func (g *PullRequest) Post(_ context.Context, c *reviewdog.Comment) error {
	c.Result.Diagnostic.GetLocation().Path = filepath.ToSlash(filepath.Join(g.wd,
		c.Result.Diagnostic.GetLocation().GetPath()))
	g.muComments.Lock()
	defer g.muComments.Unlock()
	g.postComments = append(g.postComments, c)
	return nil
}

// Flush posts comments which has not been posted yet.
func (g *PullRequest) Flush(ctx context.Context) error {
	g.muComments.Lock()
	defer g.muComments.Unlock()

	if err := g.setPostedComment(ctx); err != nil {
		return err
	}
	return g.postAsReviewComment(ctx)
}

func (g *PullRequest) postAsReviewComment(ctx context.Context) error {
	if g.fallbackToLog {
		// we don't have permission to post a review comment.
		// Fallback to GitHub Actions log as report.
		for _, c := range g.postComments {
			if err := g.logWriter.Post(ctx, c); err != nil {
				return err
			}
		}
		return g.logWriter.Flush(ctx)
	}

	postComments := g.postComments
	g.postComments = nil
	rawComments := make([]*reviewdog.Comment, 0, len(postComments))
	reviewComments := make([]*github.DraftReviewComment, 0, len(postComments))
	remaining := make([]*reviewdog.Comment, 0)
	for _, c := range postComments {
		if !c.Result.InDiffContext {
			// GitHub Review API cannot report results outside diff. If it's running
			// in GitHub Actions, fallback to GitHub Actions log as report.
			if cienv.IsInGitHubAction() {
				if err := g.logWriter.Post(ctx, c); err != nil {
					return err
				}
			}
			continue
		}
		body := buildBody(c)
		if g.postedcs.IsPosted(c, githubCommentLine(c), body) {
			// it's already posted. skip it.
			continue
		}

		// Only posts maxCommentsPerRequest comments per 1 request to avoid spammy
		// review comments. An example GitHub error if we don't limit the # of
		// review comments.
		//
		// > 403 You have triggered an abuse detection mechanism and have been
		// > temporarily blocked from content creation. Please retry your request
		// > again later.
		// https://docs.github.com/en/rest/overview/resources-in-the-rest-api?apiVersion=2022-11-28#rate-limiting
		if len(reviewComments) >= maxCommentsPerRequest {
			remaining = append(remaining, c)
			continue
		}
		reviewComments = append(reviewComments, buildDraftReviewComment(c, body))
	}
	if err := g.logWriter.Flush(ctx); err != nil {
		return err
	}

	if len(reviewComments) > 0 {
		// send review comments to GitHub.
		review := &github.PullRequestReviewRequest{
			CommitID: &g.sha,
			Event:    github.String("COMMENT"),
			Comments: reviewComments,
			Body:     github.String(g.remainingCommentsSummary(remaining)),
		}
		_, _, err := g.cli.PullRequests.CreateReview(ctx, g.owner, g.repo, g.pr, review)
		if err != nil {
			log.Printf("reviewdog: failed to post a review comment: %v", err)
			// GitHub returns 403 or 404 if we don't have permission to post a review comment.
			// fallback to log message in this case.
			if isPermissionError(err) && cienv.IsInGitHubAction() {
				goto FALLBACK
			}
			return err
		}
	}

	return nil

FALLBACK:
	// fallback to GitHub Actions log as report.
	fmt.Fprintln(os.Stderr, `reviewdog: This GitHub Token doesn't have write permission of Review API [1],
so reviewdog will report results via logging command [2] and create annotations similar to
github-pr-check reporter as a fallback.
[1]: https://docs.github.com/en/actions/reference/events-that-trigger-workflows#pull_request_target
[2]: https://docs.github.com/en/actions/using-workflows/workflow-commands-for-github-actions`)
	g.fallbackToLog = true

	for _, c := range rawComments {
		if err := g.logWriter.Post(ctx, c); err != nil {
			return err
		}
	}
	return g.logWriter.Flush(ctx)
}

// Document: https://docs.github.com/en/rest/reference/pulls#create-a-review-comment-for-a-pull-request
func buildDraftReviewComment(c *reviewdog.Comment, body string) *github.DraftReviewComment {
	loc := c.Result.Diagnostic.GetLocation()
	startLine, endLine := githubCommentLineRange(c)
	r := &github.DraftReviewComment{
		Path: github.String(loc.GetPath()),
		Side: github.String("RIGHT"),
		Body: github.String(body),
		Line: github.Int(endLine),
	}
	// GitHub API: Start line must precede the end line.
	if startLine < endLine {
		r.StartSide = github.String("RIGHT")
		r.StartLine = github.Int(startLine)
	}
	return r
}

// line represents end line if it's a multiline comment in GitHub, otherwise
// it's start line.
// Document: https://docs.github.com/en/rest/reference/pulls#create-a-review-comment-for-a-pull-request
func githubCommentLine(c *reviewdog.Comment) int {
	if !c.Result.InDiffContext {
		return 0
	}
	_, end := githubCommentLineRange(c)
	return end
}

func githubCommentLineRange(c *reviewdog.Comment) (start int, end int) {
	// Prefer first suggestion line range to diagnostic location if available so
	// that reviewdog can post code suggestion as well when the line ranges are
	// different between the diagnostic location and its suggestion.
	if c.Result.FirstSuggestionInDiffContext && len(c.Result.Diagnostic.GetSuggestions()) > 0 {
		s := c.Result.Diagnostic.GetSuggestions()[0]
		startLine := s.GetRange().GetStart().GetLine()
		endLine := s.GetRange().GetEnd().GetLine()
		if endLine == 0 {
			endLine = startLine
		}
		return int(startLine), int(endLine)
	}
	loc := c.Result.Diagnostic.GetLocation()
	startLine := loc.GetRange().GetStart().GetLine()
	endLine := loc.GetRange().GetEnd().GetLine()
	if endLine == 0 {
		endLine = startLine
	}
	return int(startLine), int(endLine)
}

func (g *PullRequest) remainingCommentsSummary(remaining []*reviewdog.Comment) string {
	if len(remaining) == 0 {
		return ""
	}
	perTool := make(map[string][]*reviewdog.Comment)
	for _, c := range remaining {
		perTool[c.ToolName] = append(perTool[c.ToolName], c)
	}
	var sb strings.Builder
	sb.WriteString("Remaining comments which cannot be posted as a review comment to avoid GitHub Rate Limit\n")
	sb.WriteString("\n")
	for tool, comments := range perTool {
		sb.WriteString("<details>\n")
		sb.WriteString(fmt.Sprintf("<summary>%s</summary>\n", tool))
		sb.WriteString("\n")
		for _, c := range comments {
			sb.WriteString(githubutils.LinkedMarkdownDiagnostic(g.owner, g.repo, g.sha, c.Result.Diagnostic))
			sb.WriteString("\n")
		}
		sb.WriteString("</details>\n")
	}
	return sb.String()
}

// setPostedComment get posted comments from GitHub.
func (g *PullRequest) setPostedComment(ctx context.Context) error {
	g.postedcs = make(commentutil.PostedComments)
	cs, err := g.comment(ctx)
	if err != nil {
		return err
	}
	for _, c := range cs {
		if c.Line == nil || c.Path == nil || c.Body == nil || c.SubjectType == nil {
			continue
		}
		var line int
		if c.GetSubjectType() == "line" {
			line = c.GetLine()
		}
		g.postedcs.AddPostedComment(c.GetPath(), line, c.GetBody())
	}
	return nil
}

// Diff returns a diff of PullRequest.
func (g *PullRequest) Diff(ctx context.Context) ([]byte, error) {
	opt := github.RawOptions{Type: github.Diff}
	d, resp, err := g.cli.PullRequests.GetRaw(ctx, g.owner, g.repo, g.pr, opt)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotAcceptable {
			log.Print("fallback to use git command")
			return g.diffUsingGitCommand(ctx)
		}

		return nil, err
	}
	return []byte(d), nil
}

// diffUsingGitCommand returns a diff of PullRequest using git command.
func (g *PullRequest) diffUsingGitCommand(ctx context.Context) ([]byte, error) {
	pr, _, err := g.cli.PullRequests.Get(ctx, g.owner, g.repo, g.pr)
	if err != nil {
		return nil, err
	}

	headSha := pr.GetHead().GetSHA()

	commits, _, err := g.cli.Repositories.CompareCommits(ctx, g.owner, g.repo, headSha, pr.GetBase().GetSHA(), nil)
	if err != nil {
		return nil, err
	}
	if commits == nil || commits.MergeBaseCommit == nil || commits.MergeBaseCommit.SHA == nil {
		return nil, errors.New("cannot to get merge base commit")
	}

	mergeBase := *commits.MergeBaseCommit.SHA

	bytes, err := exec.Command("git", "diff", "--find-renames", mergeBase, headSha).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to run git diff: %s%w", bytes, err)
	}

	return bytes, nil
}

// Strip returns 1 as a strip of git diff.
func (g *PullRequest) Strip() int {
	return 1
}

func (g *PullRequest) comment(ctx context.Context) ([]*github.PullRequestComment, error) {
	// https://developer.github.com/v3/guides/traversing-with-pagination/
	opts := &github.PullRequestListCommentsOptions{
		ListOptions: github.ListOptions{
			PerPage: 100,
		},
	}
	comments, err := listAllPullRequestsComments(ctx, g.cli, g.owner, g.repo, g.pr, opts)
	if err != nil {
		return nil, err
	}
	return comments, nil
}

func listAllPullRequestsComments(ctx context.Context, cli *github.Client,
	owner, repo string, pr int, opts *github.PullRequestListCommentsOptions) ([]*github.PullRequestComment, error) {
	comments, resp, err := cli.PullRequests.ListComments(ctx, owner, repo, pr, opts)
	if err != nil {
		return nil, err
	}
	if resp.NextPage == 0 {
		return comments, nil
	}
	newOpts := &github.PullRequestListCommentsOptions{
		ListOptions: github.ListOptions{
			Page:    resp.NextPage,
			PerPage: opts.PerPage,
		},
	}
	restComments, err := listAllPullRequestsComments(ctx, cli, owner, repo, pr, newOpts)
	if err != nil {
		return nil, err
	}
	return append(comments, restComments...), nil
}

func buildBody(c *reviewdog.Comment) string {
	cbody := commentutil.MarkdownComment(c)
	if suggestion := buildSuggestions(c); suggestion != "" {
		cbody += "\n" + suggestion
	}
	return cbody
}

func buildSuggestions(c *reviewdog.Comment) string {
	var sb strings.Builder
	for _, s := range c.Result.Diagnostic.GetSuggestions() {
		txt, err := buildSingleSuggestion(c, s)
		if err != nil {
			sb.WriteString(invalidSuggestionPre + err.Error() + invalidSuggestionPost + "\n")
			continue
		}
		sb.WriteString(txt)
		sb.WriteString("\n")
	}
	return sb.String()
}

func buildSingleSuggestion(c *reviewdog.Comment, s *rdf.Suggestion) (string, error) {
	start := s.GetRange().GetStart()
	startLine := int(start.GetLine())
	end := s.GetRange().GetEnd()
	endLine := int(end.GetLine())
	if endLine == 0 {
		endLine = startLine
	}
	gStart, gEnd := githubCommentLineRange(c)
	if startLine != gStart || endLine != gEnd {
		return "", fmt.Errorf("GitHub comment range and suggestion line range must be same. L%d-L%d v.s. L%d-L%d",
			gStart, gEnd, startLine, endLine)
	}
	if start.GetColumn() > 0 || end.GetColumn() > 0 {
		return buildNonLineBasedSuggestion(c, s)
	}

	txt := s.GetText()
	backticks := commentutil.GetCodeFenceLength(txt)

	var sb strings.Builder
	sb.Grow(backticks + len("suggestion\n") + len(txt) + len("\n") + backticks)
	commentutil.WriteCodeFence(&sb, backticks)
	sb.WriteString("suggestion\n")
	if txt != "" {
		sb.WriteString(txt)
		sb.WriteString("\n")
	}
	commentutil.WriteCodeFence(&sb, backticks)
	return sb.String(), nil
}

func buildNonLineBasedSuggestion(c *reviewdog.Comment, s *rdf.Suggestion) (string, error) {
	sourceLines := c.Result.SourceLines
	if len(sourceLines) == 0 {
		return "", errors.New("source lines are not available")
	}
	start := s.GetRange().GetStart()
	end := s.GetRange().GetEnd()
	startLineContent, err := getSourceLine(sourceLines, int(start.GetLine()))
	if err != nil {
		return "", err
	}
	endLineContent, err := getSourceLine(sourceLines, int(end.GetLine()))
	if err != nil {
		return "", err
	}

	txt := startLineContent[:max(start.GetColumn()-1, 0)] + s.GetText() + endLineContent[max(end.GetColumn()-1, 0):]
	backticks := commentutil.GetCodeFenceLength(txt)

	var sb strings.Builder
	sb.Grow(backticks + len("suggestion\n") + len(txt) + len("\n") + backticks)
	commentutil.WriteCodeFence(&sb, backticks)
	sb.WriteString("suggestion\n")
	sb.WriteString(txt)
	sb.WriteString("\n")
	commentutil.WriteCodeFence(&sb, backticks)
	return sb.String(), nil
}

func getSourceLine(sourceLines map[int]string, line int) (string, error) {
	lineContent, ok := sourceLines[line]
	if !ok {
		return "", fmt.Errorf("source line (L=%d) is not available for this suggestion", line)
	}
	return lineContent, nil
}
