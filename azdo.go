package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

const (
	apiVersion = "7.1"

	// Limits on what is sent to the AI. Anything over them is reported in
	// prDiff.Omitted rather than silently dropped. The diff budget is sized
	// for a local model: roughly 40K tokens.
	maxChangedFiles = 100
	maxFileBytes    = 256 << 10
	maxDiffBytes    = 128 << 10
)

// prDiff is the textual content of a pull request's changes.
type prDiff struct {
	Iteration int          // the PR iteration the diff was taken from
	Text      string       // unified diff of all included files
	Files     []fileChange // the same files, one by one, for commenting on
	Omitted   []string     // "path (change type, reason)" for every change left out
}

// fileChange is one changed text file of a pull request.
type fileChange struct {
	Path             string
	ChangeType       string
	ChangeTrackingID int          // ties a comment to this change across iterations
	Numbered         string       // diff with the new file's line number on each line
	Lines            map[int]bool // new-file line numbers that appear in Numbered
}

// azdoClient talks to the Azure DevOps Git REST API with a personal access
// token. Requests only ever go to the configured organization URL, never to a
// URL taken from a webhook payload.
type azdoClient struct {
	orgURL string
	auth   string
	http   *http.Client
}

func newAzdoClient(orgURL, pat string) *azdoClient {
	return &azdoClient{
		orgURL: orgURL,
		auth:   "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+pat)),
		http:   &http.Client{},
	}
}

func (c *azdoClient) repoURL(pr pullRequest, path string, query url.Values) string {
	if query == nil {
		query = url.Values{}
	}
	query.Set("api-version", apiVersion)
	return fmt.Sprintf("%s/%s/_apis/git/repositories/%s/%s?%s", c.orgURL,
		url.PathEscape(pr.Repository.Project.ID), url.PathEscape(pr.Repository.ID), path, query.Encode())
}

func (c *azdoClient) do(ctx context.Context, method, url, accept string, body any) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", accept)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return data, nil
	case http.StatusNonAuthoritativeInfo, http.StatusUnauthorized:
		// An invalid or expired PAT gets a 203 with an HTML sign-in page.
		return nil, fmt.Errorf("%s %s: %s: Azure DevOps rejected the credentials; check AZDO_PAT and its scopes",
			method, req.URL.Path, resp.Status)
	default:
		return nil, fmt.Errorf("%s %s: %s: %.200s", method, req.URL.Path, resp.Status,
			strings.Join(strings.Fields(string(data)), " "))
	}
}

func (c *azdoClient) getJSON(ctx context.Context, url string, out any) error {
	data, err := c.do(ctx, http.MethodGet, url, "application/json", nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// Diff builds a unified diff of the pull request's latest iteration against
// the commit it branched from.
func (c *azdoClient) Diff(ctx context.Context, pr pullRequest) (prDiff, error) {
	var iterations struct {
		Value []struct {
			ID int `json:"id"`
		} `json:"value"`
	}
	if err := c.getJSON(ctx, c.repoURL(pr, fmt.Sprintf("pullRequests/%d/iterations", pr.ID), nil), &iterations); err != nil {
		return prDiff{}, err
	}
	if len(iterations.Value) == 0 {
		return prDiff{}, fmt.Errorf("pull request %d has no iterations", pr.ID)
	}
	latest := iterations.Value[len(iterations.Value)-1].ID

	var changes struct {
		ChangeEntries []struct {
			ChangeTrackingID int    `json:"changeTrackingId"`
			ChangeType       string `json:"changeType"`
			Item             struct {
				Path             string `json:"path"`
				ObjectID         string `json:"objectId"`
				OriginalObjectID string `json:"originalObjectId"`
				IsFolder         bool   `json:"isFolder"`
			} `json:"item"`
			OriginalPath string `json:"originalPath"`
		} `json:"changeEntries"`
		NextTop int `json:"nextTop"`
	}
	changesURL := c.repoURL(pr, fmt.Sprintf("pullRequests/%d/iterations/%d/changes", pr.ID, latest),
		url.Values{"$top": {fmt.Sprint(maxChangedFiles)}})
	if err := c.getJSON(ctx, changesURL, &changes); err != nil {
		return prDiff{}, err
	}

	diff := prDiff{Iteration: latest}
	var text strings.Builder
	for _, ch := range changes.ChangeEntries {
		item := ch.Item
		if item.IsFolder {
			continue
		}
		if text.Len() > maxDiffBytes {
			diff.Omitted = append(diff.Omitted, fmt.Sprintf("%s (%s, pull request too large)", item.Path, ch.ChangeType))
			continue
		}
		before, reason, err := c.blobText(ctx, pr, item.OriginalObjectID)
		if err != nil {
			return prDiff{}, err
		}
		var after string
		if reason == "" {
			after, reason, err = c.blobText(ctx, pr, item.ObjectID)
			if err != nil {
				return prDiff{}, err
			}
		}
		if reason != "" {
			diff.Omitted = append(diff.Omitted, fmt.Sprintf("%s (%s, %s)", item.Path, ch.ChangeType, reason))
			continue
		}
		fromPath := item.Path
		if ch.OriginalPath != "" {
			fromPath = ch.OriginalPath
		}
		fileDiff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A:        difflib.SplitLines(before),
			B:        difflib.SplitLines(after),
			FromFile: "a" + fromPath,
			ToFile:   "b" + item.Path,
			Context:  5,
		})
		if err != nil {
			return prDiff{}, err
		}
		fmt.Fprintf(&text, "# %s: %s\n%s\n", ch.ChangeType, item.Path, fileDiff)

		numbered, lines := numberedDiff(before, after)
		diff.Files = append(diff.Files, fileChange{
			Path:             item.Path,
			ChangeType:       ch.ChangeType,
			ChangeTrackingID: ch.ChangeTrackingID,
			Numbered:         numbered,
			Lines:            lines,
		})
	}
	if changes.NextTop > 0 {
		diff.Omitted = append(diff.Omitted, fmt.Sprintf("changes beyond the first %d files", maxChangedFiles))
	}
	diff.Text = text.String()
	return diff, nil
}

// numberedDiff renders a diff in which every line that exists in the new file
// starts with its line number, so a model can cite lines without doing hunk
// arithmetic. It also returns the set of line numbers shown.
//
//	12   unchanged line
//	   - removed line
//	13 + added line
func numberedDiff(before, after string) (string, map[int]bool) {
	a, b := splitLines(before), splitLines(after)
	lines := make(map[int]bool)
	var out strings.Builder
	write := func(number int, marker string, line string) {
		if number > 0 {
			lines[number] = true
			fmt.Fprintf(&out, "%5d %s %s", number, marker, line)
		} else {
			fmt.Fprintf(&out, "      %s %s", marker, line)
		}
		if !strings.HasSuffix(line, "\n") {
			out.WriteByte('\n')
		}
	}
	for i, group := range difflib.NewMatcher(a, b).GetGroupedOpCodes(5) {
		if i > 0 {
			out.WriteString("      ...\n")
		}
		for _, op := range group {
			if op.Tag == 'e' {
				for j := op.J1; j < op.J2; j++ {
					write(j+1, " ", b[j])
				}
				continue
			}
			for k := op.I1; k < op.I2; k++ { // 'd' and the old side of 'r'
				write(0, "-", a[k])
			}
			for j := op.J1; j < op.J2; j++ { // 'i' and the new side of 'r'
				write(j+1, "+", b[j])
			}
		}
	}
	return out.String(), lines
}

// splitLines splits s after each newline. Unlike difflib.SplitLines it adds
// no empty last line, which would throw the line numbers off by one.
func splitLines(s string) []string {
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// blobText returns a blob's content. An empty id (the missing side of an add
// or delete) yields empty content. A non-empty reason means the blob is not
// usable as text.
func (c *azdoClient) blobText(ctx context.Context, pr pullRequest, id string) (content, reason string, err error) {
	if id == "" {
		return "", "", nil
	}
	data, err := c.do(ctx, http.MethodGet,
		c.repoURL(pr, "blobs/"+url.PathEscape(id), url.Values{"$format": {"octetstream"}}),
		"application/octet-stream", nil)
	if err != nil {
		return "", "", err
	}
	switch {
	case len(data) > maxFileBytes:
		return "", "file too large", nil
	case bytes.IndexByte(data, 0) >= 0:
		return "", "binary file", nil
	}
	return string(data), "", nil
}

// UpdatePR replaces the pull request's title and description.
func (c *azdoClient) UpdatePR(ctx context.Context, pr pullRequest, title, description string) error {
	_, err := c.do(ctx, http.MethodPatch, c.repoURL(pr, fmt.Sprintf("pullrequests/%d", pr.ID), nil),
		"application/json", map[string]string{"title": title, "description": description})
	return err
}

// threadStatus is an Azure DevOps comment thread status. Active threads count
// against a "comments must be resolved" branch policy; closed ones don't.
type threadStatus int

const (
	threadActive threadStatus = 1 // something for the author to resolve
	threadClosed threadStatus = 4 // informational: already resolved
)

// PostComment adds a new comment thread to the pull request.
func (c *azdoClient) PostComment(ctx context.Context, pr pullRequest, status threadStatus, markdown string) error {
	return c.postThread(ctx, pr, status, markdown, nil)
}

// PostFileComment adds an active comment thread on a changed file. A line that is in
// file.Lines anchors the thread to that line of the new file; any other line
// number makes it a comment on the file as a whole.
func (c *azdoClient) PostFileComment(ctx context.Context, pr pullRequest, iteration int, file fileChange, line int, markdown string) error {
	threadContext := map[string]any{"filePath": file.Path}
	if file.Lines[line] {
		position := map[string]int{"line": line, "offset": 1}
		threadContext["rightFileStart"], threadContext["rightFileEnd"] = position, position
	}
	return c.postThread(ctx, pr, threadActive, markdown, map[string]any{
		"threadContext": threadContext,
		"pullRequestThreadContext": map[string]any{
			"changeTrackingId": file.ChangeTrackingID,
			"iterationContext": map[string]int{
				"firstComparingIteration":  iteration,
				"secondComparingIteration": iteration,
			},
		},
	})
}

func (c *azdoClient) postThread(ctx context.Context, pr pullRequest, status threadStatus, markdown string, extra map[string]any) error {
	thread := map[string]any{
		"status": status,
		"comments": []map[string]any{{
			"parentCommentId": 0,
			"commentType":     1, // text
			"content":         markdown,
		}},
	}
	for k, v := range extra {
		thread[k] = v
	}
	_, err := c.do(ctx, http.MethodPost, c.repoURL(pr, fmt.Sprintf("pullRequests/%d/threads", pr.ID), nil),
		"application/json", thread)
	return err
}
