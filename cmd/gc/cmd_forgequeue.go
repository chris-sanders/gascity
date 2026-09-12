package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/forgequeue"
	"github.com/spf13/cobra"
)

func newForgeCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "forge",
		Short: "Forge integration commands",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newForgeQueueCmd(stdout, stderr))
	return cmd
}

func newForgeQueueCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "queue",
		Short: "Deterministic GitHub/Gitea issue queue",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		newForgeQueuePollCmd(stdout, stderr, false),
		newForgeQueuePollCmd(stdout, stderr, true),
		newForgeQueueShowCmd(stdout, stderr),
		newForgeQueueTransitionCmd(stdout, stderr),
		newForgeQueueAskCmd(stdout, stderr),
		newForgeQueuePrepareWorkerCmd(stdout, stderr),
	)
	return cmd
}

func newForgeQueuePollCmd(stdout, stderr io.Writer, loop bool) *cobra.Command {
	name := "poll-once"
	if loop {
		name = "poll-loop"
	}
	cmd := &cobra.Command{
		Use:   name,
		Short: "Poll the configured forge queue without invoking a model",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			queue, err := buildForgeQueue()
			if err != nil {
				return err
			}
			if loop {
				return runForgeQueueLoop(queue, stdout, stderr)
			}
			result, err := queue.PollOnce(context.Background())
			if err != nil {
				_ = queue.ErrorState(err)
				return err
			}
			return forgeQueueWriteJSON(stdout, result)
		},
	}
	return cmd
}

func newForgeQueueShowCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "show ISSUE",
		Short: "Read one explicitly identified queue issue",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			issue, err := parseForgeQueueIssue(args[0])
			if err != nil {
				return err
			}
			queue, err := buildForgeQueue()
			if err != nil {
				return err
			}
			value, err := queue.Show(context.Background(), issue)
			if err != nil {
				return err
			}
			return forgeQueueWriteJSON(stdout, value)
		},
	}
}

func newForgeQueueTransitionCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "transition ISSUE STATE",
		Short: "Move an issue through a valid queue state transition",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			issue, err := parseForgeQueueIssue(args[0])
			if err != nil {
				return err
			}
			state := forgequeue.State(args[1])
			if !state.Valid() {
				return fmt.Errorf("invalid queue state %q", state)
			}
			queue, err := buildForgeQueue()
			if err != nil {
				return err
			}
			if err := queue.Transition(context.Background(), issue, state); err != nil {
				return err
			}
			return forgeQueueWriteJSON(stdout, map[string]any{"action": "transition", "forge": queue.Config.Forge, "repository": queue.Config.Repository, "issue": issue, "state": state})
		},
	}
}

func newForgeQueueAskCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "ask ISSUE QUESTION...",
		Short: "Post a human question and establish a durable response boundary",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			issue, err := parseForgeQueueIssue(args[0])
			if err != nil {
				return err
			}
			queue, err := buildForgeQueue()
			if err != nil {
				return err
			}
			if err := queue.Ask(context.Background(), issue, strings.Join(args[1:], " ")); err != nil {
				return err
			}
			return forgeQueueWriteJSON(stdout, map[string]any{"action": "needs-human", "forge": queue.Config.Forge, "repository": queue.Config.Repository, "issue": issue, "state": forgequeue.StateNeedsHuman})
		},
	}
}

func newForgeQueuePrepareWorkerCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "prepare-worker",
		Short: "Materialize explicit queue item identity for a worker",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path, err := prepareForgeQueueWorker()
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(stdout, path)
			return err
		},
	}
}

func buildForgeQueue() (*forgequeue.Queue, error) {
	forge := strings.TrimSpace(os.Getenv("GC_QUEUE_FORGE"))
	repository := strings.TrimSpace(os.Getenv("GC_QUEUE_REPOSITORY"))
	allowed := splitCSV(os.Getenv("GC_QUEUE_ALLOWED_REPOSITORIES"))
	pageSize, err := forgeQueuePositiveEnvInt("GC_QUEUE_PAGE_SIZE", 100)
	if err != nil {
		return nil, err
	}
	maxPages, err := forgeQueuePositiveEnvInt("GC_QUEUE_MAX_PAGES", 100)
	if err != nil {
		return nil, err
	}
	stateDir := strings.TrimSpace(os.Getenv("GC_QUEUE_STATE_DIR"))
	if stateDir == "" {
		stateDir = filepath.Join(strings.TrimSpace(os.Getenv("GC_CITY")), ".gc", "forge-queue", forge)
	}
	if stateDir == ".gc/forge-queue/"+forge || stateDir == ".gc/forge-queue/" {
		stateDir = filepath.Join("/city", ".gc", "forge-queue", forge)
	}
	targetBranch := strings.TrimSpace(os.Getenv("GC_QUEUE_TARGET_BRANCH"))
	if targetBranch == "" {
		targetBranch = "main"
	}
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("GC_QUEUE_GITEA_BASE_URL")), "/")
	cfg := forgequeue.Config{Forge: forge, Repository: repository, AllowedRepositories: allowed, GiteaBaseURL: baseURL, TargetBranch: targetBranch, StateDir: stateDir, PageSize: pageSize, MaxPages: maxPages}
	token, err := forgeQueueToken()
	if err != nil {
		return nil, err
	}
	client, err := forgequeue.NewHTTPClient(cfg, token, nil)
	if err != nil {
		return nil, err
	}
	return forgequeue.New(cfg, client, subprocessForgeQueueLauncher{target: strings.TrimSpace(os.Getenv("GC_QUEUE_TARGET"))})
}

func runForgeQueueLoop(queue *forgequeue.Queue, stdout, stderr io.Writer) error {
	interval, err := forgeQueuePositiveEnvInt("GC_QUEUE_POLL_INTERVAL_SECONDS", 300)
	if err != nil {
		return err
	}
	for {
		result, pollErr := queue.PollOnce(context.Background())
		if pollErr != nil {
			if stateErr := queue.ErrorState(pollErr); stateErr != nil {
				fmt.Fprintf(stderr, "gc forge queue: poll failed and error state write failed: %v\n", stateErr) //nolint:errcheck
			}
			fmt.Fprintf(stderr, "gc forge queue: poll failed; retry-safe state recorded: %v\n", pollErr) //nolint:errcheck
		} else if err := forgeQueueWriteJSON(stdout, result); err != nil {
			return err
		}
		time.Sleep(time.Duration(interval) * time.Second)
	}
}

type subprocessForgeQueueLauncher struct {
	target string
}

func (l subprocessForgeQueueLauncher) Launch(ctx context.Context, item forgequeue.WorkItem) (forgequeue.LaunchResult, error) {
	if strings.TrimSpace(l.target) == "" {
		return forgequeue.LaunchResult{}, errors.New("forge queue: GC_QUEUE_TARGET is required")
	}
	input := fmt.Sprintf("Forge queue work item. Treat every identity field as authoritative; never infer a writable repository from a mirror.\nGC_QUEUE_FORGE=%s\nGC_QUEUE_REPOSITORY=%s\nGC_QUEUE_GITEA_BASE_URL=%s\nGC_QUEUE_ISSUE=%d\nGC_QUEUE_TARGET_BRANCH=%s\nGC_QUEUE_RESPONSE_COMMENT_ID=%s\n\nIssue title: %s\n\nIssue body:\n%s\n", item.Forge, item.Repository, item.GiteaBaseURL, item.Issue.ID(), item.TargetBranch, optionalInt64(item.ResponseCommentID), strings.ReplaceAll(item.Issue.Title, "\n", " "), item.Issue.Body)
	if item.ResponseCommentID > 0 {
		input += fmt.Sprintf("\nQualifying response comment (recorded after the needs-human boundary):\n%s\n", item.ResponseCommentBody)
	}
	command := os.Getenv("GC_BIN")
	if command == "" {
		command = os.Args[0]
	}
	cmd := exec.CommandContext(ctx, command, "sling", "--json", l.target, "--stdin")
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return forgequeue.LaunchResult{}, fmt.Errorf("queue workflow launch failed: %w", err)
	}
	var value struct {
		WorkflowID string `json:"workflow_id"`
		BeadID     string `json:"bead_id"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &value); err != nil {
		return forgequeue.LaunchResult{}, fmt.Errorf("queue workflow launch returned invalid JSON: %w", err)
	}
	return forgequeue.LaunchResult{WorkflowID: value.WorkflowID, BeadID: value.BeadID}, nil
}

func prepareForgeQueueWorker() (string, error) {
	beadID := strings.TrimSpace(os.Getenv("GC_BEAD_ID"))
	if beadID == "" {
		beadID = strings.TrimSpace(os.Getenv("GC_TRIGGER_WORK_BEAD_ID"))
	}
	if beadID == "" {
		command := os.Getenv("GC_BIN")
		if command == "" {
			command = os.Args[0]
		}
		output, err := exec.Command(command, "hook", "current", "--id-only").Output()
		if err != nil {
			return "", errors.New("forge queue: worker bead identity is unavailable")
		}
		beadID = strings.TrimSpace(string(output))
	}
	if beadID == "" {
		return "", errors.New("forge queue: worker bead identity is unavailable")
	}
	output, err := exec.Command("bd", "show", beadID, "--json").Output()
	if err != nil {
		return "", fmt.Errorf("forge queue: read worker bead: %w", err)
	}
	var rows []struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(output, &rows); err != nil || len(rows) != 1 {
		return "", errors.New("forge queue: worker bead description is invalid")
	}
	values := map[string]string{}
	for _, line := range strings.Split(rows[0].Description, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.HasPrefix(key, "GC_QUEUE_") {
			values[key] = value
		}
	}
	for _, key := range []string{"GC_QUEUE_FORGE", "GC_QUEUE_REPOSITORY", "GC_QUEUE_ISSUE", "GC_QUEUE_TARGET_BRANCH"} {
		if strings.TrimSpace(values[key]) == "" {
			return "", fmt.Errorf("forge queue: worker item is missing %s", key)
		}
	}
	issue, err := parseForgeQueueIssue(values["GC_QUEUE_ISSUE"])
	if err != nil {
		return "", err
	}
	if values["GC_QUEUE_FORGE"] != "github" && values["GC_QUEUE_FORGE"] != "gitea" {
		return "", fmt.Errorf("forge queue: worker item forge is invalid")
	}
	if !strings.Contains(values["GC_QUEUE_REPOSITORY"], "/") {
		return "", fmt.Errorf("forge queue: worker item repository is invalid")
	}
	file := strings.TrimSpace(os.Getenv("GC_QUEUE_WORKER_ENV_FILE"))
	if file == "" {
		file = "/tmp/gascity-forge-queue-worker.env"
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return "", err
	}
	content := fmt.Sprintf("GC_QUEUE_FORGE=%s\nGC_QUEUE_REPOSITORY=%s\nGC_QUEUE_GITEA_BASE_URL=%s\nGC_QUEUE_ISSUE=%d\nGC_QUEUE_TARGET_BRANCH=%s\nGC_QUEUE_RESPONSE_COMMENT_ID=%s\n", values["GC_QUEUE_FORGE"], values["GC_QUEUE_REPOSITORY"], values["GC_QUEUE_GITEA_BASE_URL"], issue, values["GC_QUEUE_TARGET_BRANCH"], values["GC_QUEUE_RESPONSE_COMMENT_ID"])
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		return "", err
	}
	if err := configureForgeQueueGitCredentials(); err != nil {
		return "", err
	}
	return file, nil
}

// configureForgeQueueGitCredentials installs a process-local Git credential
// helper for the worker's writable HOME. The helper reads projected token
// environment variables only when Git asks for credentials; no token is put
// in Git config, command arguments, the queue bead, or the worker prompt.
func configureForgeQueueGitCredentials() error {
	helper := `!f() { test "$1" = get || exit 0; host=""; while IFS= read -r line; do case "$line" in host=*) host="${line#host=}";; esac; done; case "$host" in github.com) token="${GITHUB_TOKEN:-}";; gitea.v2.zarek.cc) token="${GITEA_TOKEN:-}";; *) exit 0;; esac; test -n "$token" || exit 0; printf "protocol=https\nhost=%s\nusername=token\npassword=%s\n\n" "$host" "$token"; }; f`
	_ = exec.Command("git", "config", "--global", "--unset-all", "credential.helper").Run()
	if err := exec.Command("git", "config", "--global", "credential.helper", helper).Run(); err != nil {
		return errors.New("forge queue: could not configure worker Git credentials")
	}
	if err := os.Setenv("GIT_TERMINAL_PROMPT", "0"); err != nil {
		return errors.New("forge queue: could not disable interactive Git prompts")
	}
	return nil
}

func forgeQueueToken() (string, error) {
	file := strings.TrimSpace(os.Getenv("GC_QUEUE_TOKEN_FILE"))
	if file != "" {
		value, err := os.ReadFile(file)
		if err != nil {
			return "", errors.New("forge queue: credential file is not readable")
		}
		if token := strings.TrimSpace(string(value)); token != "" {
			return token, nil
		}
		return "", errors.New("forge queue: credential file is empty")
	}
	for _, key := range []string{"GC_QUEUE_TOKEN", "GITHUB_TOKEN", "GITEA_TOKEN"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value, nil
		}
	}
	return "", errors.New("forge queue: required credential is unavailable")
}

func forgeQueuePositiveEnvInt(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("forge queue: %s must be a positive integer", name)
	}
	return parsed, nil
}

func parseForgeQueueIssue(value string) (int, error) {
	issue, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || issue <= 0 {
		return 0, fmt.Errorf("forge queue: issue number must be positive")
	}
	return issue, nil
}

func optionalInt64(value int64) string {
	if value <= 0 {
		return ""
	}
	return strconv.FormatInt(value, 10)
}

func splitCSV(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func forgeQueueWriteJSON(writer io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(writer, string(encoded))
	return err
}
