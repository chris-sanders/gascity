package forgequeue

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestHTTPClientUsesProviderAuthAndRecoversAmbiguousCommentWrite(t *testing.T) {
	var requests []*http.Request
	client := &HTTPClient{
		BaseURL: "https://gitea.example/api/v1",
		Token:   "fixture-token",
		Forge:   "gitea",
		Repo:    "owner/repo",
		Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests = append(requests, req)
			if req.Method == http.MethodPost {
				return response(http.StatusInternalServerError, "connection closed after acceptance"), nil
			}
			return response(http.StatusOK, `[{"id":42,"body":"queue question"}]`), nil
		})},
	}

	comment, err := client.CreateComment(context.Background(), 7, "queue question")
	if err != nil {
		t.Fatal(err)
	}
	if comment.ID != 42 {
		t.Fatalf("comment = %#v, want read-back ID 42", comment)
	}
	if len(requests) != 2 || requests[0].Method != http.MethodPost || requests[1].Method != http.MethodGet {
		t.Fatalf("requests = %v, want one POST followed by one GET", requestMethods(requests))
	}
	if got := requests[0].Header.Get("Authorization"); got != "token fixture-token" {
		t.Fatalf("Gitea Authorization = %q, want token scheme", got)
	}
	if got := requests[0].URL.Path; got != "/api/v1/repos/owner/repo/issues/7/comments" {
		t.Fatalf("comment POST path = %q", got)
	}
}

func TestHTTPClientUsesBearerForGitHub(t *testing.T) {
	var request *http.Request
	client := &HTTPClient{
		BaseURL: "https://api.github.com",
		Token:   "fixture-token",
		Forge:   "github",
		Repo:    "owner/repo",
		Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			request = req
			return response(http.StatusOK, `[]`), nil
		})},
	}

	if _, err := client.ListIssues(context.Background(), 1, 100); err != nil {
		t.Fatal(err)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer fixture-token" {
		t.Fatalf("GitHub Authorization = %q, want Bearer scheme", got)
	}
	if got := request.URL.Query().Get("per_page"); got != "100" {
		t.Fatalf("GitHub per_page = %q, want 100", got)
	}
}

func TestConfigRejectsGiteaOutsideHTTPSAllowlist(t *testing.T) {
	cases := []Config{
		{Forge: "gitea", Repository: "owner/repo", AllowedRepositories: []string{"other/repo"}, GiteaBaseURL: "https://gitea.example", TargetBranch: "master", StateDir: t.TempDir(), PageSize: 1, MaxPages: 1},
		{Forge: "gitea", Repository: "owner/repo", AllowedRepositories: []string{"owner/repo"}, GiteaBaseURL: "http://gitea.example", TargetBranch: "master", StateDir: t.TempDir(), PageSize: 1, MaxPages: 1},
	}
	for _, cfg := range cases {
		if err := cfg.Validate(); err == nil {
			t.Errorf("Config.Validate(%#v) succeeded, want rejection", cfg)
		}
	}
}

func requestMethods(requests []*http.Request) []string {
	methods := make([]string, 0, len(requests))
	for _, req := range requests {
		methods = append(methods, req.Method)
	}
	return methods
}
