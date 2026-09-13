package forgequeue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// HTTPClient is the small REST surface used by the queue. It deliberately
// keeps GitHub and Gitea behind the same Forge interface so queue semantics do
// not depend on a provider-specific command-line client.
type HTTPClient struct {
	BaseURL string
	Token   string
	Forge   string
	Repo    string
	Client  *http.Client
}

func NewHTTPClient(cfg Config, token string, client *http.Client) (*HTTPClient, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	base := "https://api.github.com"
	if cfg.Forge == "gitea" {
		base = strings.TrimRight(cfg.GiteaBaseURL, "/") + "/api/v1"
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPClient{BaseURL: base, Token: token, Forge: cfg.Forge, Repo: cfg.Repository, Client: client}, nil
}

func (c *HTTPClient) endpoint(suffix string, query url.Values) string {
	value := strings.TrimRight(c.BaseURL, "/") + "/repos/" + c.Repo + "/" + strings.TrimLeft(suffix, "/")
	if encoded := query.Encode(); encoded != "" {
		value += "?" + encoded
	}
	return value
}

func (c *HTTPClient) do(ctx context.Context, method, suffix string, query url.Values, requestBody any, response any) error {
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(suffix, query), body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		if c.Forge == "github" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		} else {
			req.Header.Set("Authorization", "token "+c.Token)
		}
	}
	res, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("forge API %s %s: HTTP %d: %s", method, suffix, res.StatusCode, strings.TrimSpace(string(data)))
	}
	if response == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	}
	if err := json.NewDecoder(res.Body).Decode(response); err != nil {
		return fmt.Errorf("decode forge API %s %s: %w", method, suffix, err)
	}
	return nil
}

func (c *HTTPClient) ListIssues(ctx context.Context, page, limit int) ([]Issue, error) {
	query := url.Values{"state": {"open"}, "page": {strconv.Itoa(page)}}
	if c.Forge == "github" {
		query.Set("per_page", strconv.Itoa(limit))
	} else {
		query.Set("limit", strconv.Itoa(limit))
	}
	var result []Issue
	err := c.do(ctx, http.MethodGet, "issues", query, nil, &result)
	return result, err
}

func (c *HTTPClient) GetIssue(ctx context.Context, number int) (Issue, error) {
	var result Issue
	err := c.do(ctx, http.MethodGet, "issues/"+strconv.Itoa(number), nil, nil, &result)
	return result, err
}

func (c *HTTPClient) ListComments(ctx context.Context, number, page, limit int) ([]Comment, error) {
	query := url.Values{"page": {strconv.Itoa(page)}}
	if c.Forge == "github" {
		query.Set("per_page", strconv.Itoa(limit))
	} else {
		query.Set("limit", strconv.Itoa(limit))
	}
	var result []Comment
	err := c.do(ctx, http.MethodGet, "issues/"+strconv.Itoa(number)+"/comments", query, nil, &result)
	return result, err
}

func (c *HTTPClient) ListLabels(ctx context.Context, page, limit int) ([]Label, error) {
	query := url.Values{"page": {strconv.Itoa(page)}}
	if c.Forge == "github" {
		query.Set("per_page", strconv.Itoa(limit))
	} else {
		query.Set("limit", strconv.Itoa(limit))
	}
	var result []Label
	err := c.do(ctx, http.MethodGet, "labels", query, nil, &result)
	return result, err
}

func (c *HTTPClient) CreateLabel(ctx context.Context, name, color string) error {
	return c.do(ctx, http.MethodPost, "labels", nil, map[string]string{"name": name, "color": color}, nil)
}

func (c *HTTPClient) SetIssueLabels(ctx context.Context, number int, desired State, labels []Label) error {
	if c.Forge == "github" {
		for _, label := range labels {
			if State(label.Name).Valid() {
				encoded := url.PathEscape(label.Name)
				if err := c.do(ctx, http.MethodDelete, "issues/"+strconv.Itoa(number)+"/labels/"+encoded, nil, nil, nil); err != nil {
					return err
				}
			}
		}
		return c.do(ctx, http.MethodPost, "issues/"+strconv.Itoa(number)+"/labels", nil, map[string][]string{"labels": {string(desired)}}, nil)
	}
	for _, label := range labels {
		if State(label.Name).Valid() && label.ID > 0 {
			if err := c.do(ctx, http.MethodDelete, "issues/"+strconv.Itoa(number)+"/labels/"+strconv.FormatInt(label.ID, 10), nil, nil, nil); err != nil {
				return err
			}
		}
	}
	var labelID int64
	for _, label := range labels {
		if label.Name == string(desired) {
			labelID = label.ID
			break
		}
	}
	if labelID == 0 {
		return fmt.Errorf("queue label %s is not available", desired)
	}
	return c.do(ctx, http.MethodPost, "issues/"+strconv.Itoa(number)+"/labels", nil, map[string][]int64{"labels": {labelID}}, nil)
}

func (c *HTTPClient) CreateComment(ctx context.Context, number int, body string) (Comment, error) {
	var result Comment
	err := c.do(ctx, http.MethodPost, "issues/"+strconv.Itoa(number)+"/comments", nil, map[string]string{"body": body}, &result)
	if err == nil {
		return result, nil
	}
	// A timed-out POST can have reached the forge. Read back the exact marker
	// before reporting failure so callers never blind-retry a non-idempotent
	// write. The caller can safely retry when this returns no exact identity.
	comments, readErr := c.listAllComments(ctx, number, 100, 100)
	if readErr == nil {
		for _, comment := range comments {
			if comment.Body == body {
				return comment, nil
			}
		}
	}
	return Comment{}, err
}

func (c *HTTPClient) listAllComments(ctx context.Context, number, limit, maxPages int) ([]Comment, error) {
	all := make([]Comment, 0)
	for page := 1; page <= maxPages; page++ {
		comments, err := c.ListComments(ctx, number, page, limit)
		if err != nil {
			return nil, err
		}
		all = append(all, comments...)
		if len(comments) < limit {
			return all, nil
		}
	}
	return nil, fmt.Errorf("forge queue: comment readback exceeded bounded page limit")
}

var _ Forge = (*HTTPClient)(nil)
