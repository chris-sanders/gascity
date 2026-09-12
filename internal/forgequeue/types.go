// Package forgequeue contains the deterministic, forge-neutral issue queue.
//
// The queue owns discovery, claim arbitration, durable human-response fences,
// and forge state transitions. It never invokes a model while polling. A
// caller supplies the launcher used after a claim has been recorded.
package forgequeue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	Schema = "gc-queue-v3"

	StateQueued     State = "gc:queued"
	StateWorking    State = "gc:working"
	StateNeedsHuman State = "gc:needs-human"
	StateBlocked    State = "gc:blocked"
	StateDone       State = "gc:done"
)

var ErrNoAction = errors.New("forge queue: no action")

type State string

func (s State) Valid() bool {
	switch s {
	case StateQueued, StateWorking, StateNeedsHuman, StateBlocked, StateDone:
		return true
	default:
		return false
	}
}

type Label struct {
	ID   int64  `json:"id,omitempty"`
	Name string `json:"name"`
}

type Issue struct {
	Number      int     `json:"number"`
	Index       int     `json:"index"`
	Title       string  `json:"title"`
	Body        string  `json:"body"`
	Labels      []Label `json:"labels"`
	PullRequest any     `json:"pull_request,omitempty"`
}

func (i Issue) ID() int {
	if i.Number != 0 {
		return i.Number
	}
	return i.Index
}

type Comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

type WorkItem struct {
	Forge               string
	Repository          string
	GiteaBaseURL        string
	Issue               Issue
	TargetBranch        string
	ResponseCommentID   int64
	ResponseCommentBody string
}

type LaunchResult struct {
	WorkflowID string `json:"workflow_id,omitempty"`
	BeadID     string `json:"bead_id,omitempty"`
}

type Launcher interface {
	Launch(context.Context, WorkItem) (LaunchResult, error)
}

type Forge interface {
	ListIssues(context.Context, int, int) ([]Issue, error)
	GetIssue(context.Context, int) (Issue, error)
	ListComments(context.Context, int, int, int) ([]Comment, error)
	ListLabels(context.Context, int, int) ([]Label, error)
	CreateLabel(context.Context, string, string) error
	SetIssueLabels(context.Context, int, State, []Label) error
	CreateComment(context.Context, int, string) (Comment, error)
}

type Config struct {
	Forge               string
	Repository          string
	AllowedRepositories []string
	GiteaBaseURL        string
	TargetBranch        string
	StateDir            string
	PageSize            int
	MaxPages            int
	Marker              string
	Now                 func() time.Time
}

func (c Config) Validate() error {
	if c.Forge != "github" && c.Forge != "gitea" {
		return fmt.Errorf("forge queue: forge must be github or gitea")
	}
	parts := strings.Split(c.Repository, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return fmt.Errorf("forge queue: repository must be owner/name")
	}
	if c.Forge == "gitea" && !strings.HasPrefix(c.GiteaBaseURL, "https://") {
		return fmt.Errorf("forge queue: Gitea base URL must be an https origin")
	}
	if len(c.AllowedRepositories) > 0 && !containsString(c.AllowedRepositories, c.Repository) {
		return fmt.Errorf("forge queue: repository %q is not in the explicit forge allowlist", c.Repository)
	}
	if c.TargetBranch == "" {
		return fmt.Errorf("forge queue: target branch is required")
	}
	if c.StateDir == "" {
		return fmt.Errorf("forge queue: state directory is required")
	}
	if c.PageSize <= 0 || c.MaxPages <= 0 {
		return fmt.Errorf("forge queue: page size and max pages must be positive")
	}
	return nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == wanted {
			return true
		}
	}
	return false
}

type itemState struct {
	Schema            string     `json:"schema"`
	Forge             string     `json:"forge"`
	Repository        string     `json:"repository"`
	Issue             int        `json:"issue"`
	State             State      `json:"state"`
	ClaimID           string     `json:"claim_id,omitempty"`
	HumanCycle        int        `json:"human_cycle"`
	BoundaryCommentID int64      `json:"needs_human_boundary_id,omitempty"`
	ConsumedResponses []Consumed `json:"consumed_responses,omitempty"`
	LaunchStatus      string     `json:"launch_status,omitempty"`
	WorkflowID        string     `json:"workflow_id,omitempty"`
	BeadID            string     `json:"bead_id,omitempty"`
	LastError         string     `json:"last_error,omitempty"`
}

type Consumed struct {
	Cycle     int   `json:"cycle"`
	CommentID int64 `json:"comment_id"`
}

type PollResult struct {
	Action     string `json:"action"`
	Forge      string `json:"forge"`
	Repository string `json:"repository"`
	Issue      int    `json:"issue,omitempty"`
	State      State  `json:"state,omitempty"`
	WorkflowID string `json:"workflow_id,omitempty"`
	BeadID     string `json:"bead_id,omitempty"`
	ResponseID int64  `json:"response_comment_id,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type Queue struct {
	Config   Config
	Forge    Forge
	Launcher Launcher
	lock     *os.File
}

func New(cfg Config, forge Forge, launcher Launcher) (*Queue, error) {
	if cfg.Marker == "" {
		cfg.Marker = fmt.Sprintf("<!-- %s forge=%s repository=%s -->", Schema, cfg.Forge, cfg.Repository)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if forge == nil {
		return nil, fmt.Errorf("forge queue: forge client is nil")
	}
	if launcher == nil {
		return nil, fmt.Errorf("forge queue: launcher is nil")
	}
	return &Queue{Config: cfg, Forge: forge, Launcher: launcher}, nil
}

func (q *Queue) lockOnce() error {
	if err := os.MkdirAll(q.Config.StateDir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(q.Config.StateDir, "queue.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrBusy
		}
		return err
	}
	q.lock = f
	return nil
}

func (q *Queue) unlock() {
	if q.lock != nil {
		_ = syscall.Flock(int(q.lock.Fd()), syscall.LOCK_UN)
		_ = q.lock.Close()
		q.lock = nil
	}
}

var ErrBusy = errors.New("forge queue: another poll is running")

func (q *Queue) statePath(issue int) string {
	return filepath.Join(q.Config.StateDir, fmt.Sprintf("item-%s-%d.json", q.Config.Forge, issue))
}

func (q *Queue) readState(issue int) (*itemState, error) {
	b, err := os.ReadFile(q.statePath(issue))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state itemState
	if err := json.Unmarshal(b, &state); err != nil {
		return nil, fmt.Errorf("read queue state for issue %d: %w", issue, err)
	}
	return &state, nil
}

func (q *Queue) writeState(state itemState) error {
	if state.Schema == "" {
		state.Schema = Schema
	}
	b, err := marshalIndent(state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(q.Config.StateDir, ".state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, q.statePath(state.Issue))
}

func (q *Queue) initState(issue int, state State, claimID string, cycle int) itemState {
	return itemState{Schema: Schema, Forge: q.Config.Forge, Repository: q.Config.Repository, Issue: issue, State: state, ClaimID: claimID, HumanCycle: cycle}
}

func (q *Queue) markerBody(body string) string {
	return "<!-- gc-queue-system -->\n" + body
}

func (q *Queue) comments(ctx context.Context, issue int) ([]Comment, error) {
	all := make([]Comment, 0)
	for page := 1; page <= q.Config.MaxPages; page++ {
		comments, err := q.Forge.ListComments(ctx, issue, page, q.Config.PageSize)
		if err != nil {
			return nil, err
		}
		all = append(all, comments...)
		if len(comments) < q.Config.PageSize {
			return all, nil
		}
	}
	return nil, fmt.Errorf("forge queue: comments exceeded bounded page limit")
}

func (q *Queue) labels(ctx context.Context) ([]Label, error) {
	all := make([]Label, 0)
	for page := 1; page <= q.Config.MaxPages; page++ {
		labels, err := q.Forge.ListLabels(ctx, page, q.Config.PageSize)
		if err != nil {
			return nil, err
		}
		all = append(all, labels...)
		if len(labels) < q.Config.PageSize {
			return all, nil
		}
	}
	return nil, fmt.Errorf("forge queue: labels exceeded bounded page limit")
}

func (q *Queue) issues(ctx context.Context) ([]Issue, error) {
	all := make([]Issue, 0)
	for page := 1; page <= q.Config.MaxPages; page++ {
		issues, err := q.Forge.ListIssues(ctx, page, q.Config.PageSize)
		if err != nil {
			return nil, err
		}
		all = append(all, issues...)
		if len(issues) < q.Config.PageSize {
			return all, nil
		}
	}
	return nil, fmt.Errorf("forge queue: issues exceeded bounded page limit")
}

func (q *Queue) ensureLabels(ctx context.Context) error {
	labels, err := q.labels(ctx)
	if err != nil {
		return err
	}
	colors := map[State]string{StateQueued: "1d76db", StateWorking: "fbca04", StateNeedsHuman: "d93f0b", StateBlocked: "b60205", StateDone: "0e8a16"}
	for _, state := range []State{StateQueued, StateWorking, StateNeedsHuman, StateBlocked, StateDone} {
		found := false
		for _, label := range labels {
			if label.Name == string(state) {
				found = true
				break
			}
		}
		if !found {
			if err := q.Forge.CreateLabel(ctx, string(state), colors[state]); err != nil {
				// Re-read after a failed create: another poller may have won the
				// create race. A missing label remains a visible failure.
				refreshed, readErr := q.labels(ctx)
				if readErr != nil {
					return fmt.Errorf("create queue label %s: %v; readback failed: %w", state, err, readErr)
				}
				labels = refreshed
				if !hasLabel(labels, string(state)) {
					return fmt.Errorf("create queue label %s: %w", state, err)
				}
			}
			labels = append(labels, Label{Name: string(state)})
		}
	}
	return nil
}

func hasLabel(labels []Label, name string) bool {
	for _, label := range labels {
		if label.Name == name {
			return true
		}
	}
	return false
}

func stateOf(issue Issue) (State, error) {
	var found State
	for _, label := range issue.Labels {
		candidate := State(label.Name)
		if !candidate.Valid() {
			continue
		}
		if found != "" && found != candidate {
			return "", fmt.Errorf("issue %d has multiple queue state labels", issue.ID())
		}
		found = candidate
	}
	return found, nil
}

func systemCommentID(comments []Comment, exact string) int64 {
	for _, comment := range comments {
		if comment.Body == exact {
			return comment.ID
		}
	}
	return 0
}

func latestCycleMarker(comments []Comment, prefix string) (int, int64) {
	cycle, id := 0, int64(0)
	for _, comment := range comments {
		if !strings.HasPrefix(comment.Body, "<!-- gc-queue-system -->\n"+prefix) {
			continue
		}
		fields := strings.Fields(comment.Body)
		for _, field := range fields {
			if strings.HasPrefix(field, "cycle=") {
				value, err := strconv.Atoi(strings.TrimPrefix(field, "cycle="))
				if err == nil && (value > cycle || (value == cycle && comment.ID > id)) {
					cycle, id = value, comment.ID
				}
			}
		}
	}
	return cycle, id
}

func latestStateIntent(comments []Comment) (State, int, int64) {
	var state State
	cycle, id := 0, int64(0)
	for _, comment := range comments {
		if !strings.HasPrefix(comment.Body, "<!-- gc-queue-system -->\ngc-queue-state-intent-v3 ") {
			continue
		}
		var candidate State
		candidateCycle := 0
		for _, field := range strings.Fields(comment.Body) {
			switch {
			case strings.HasPrefix(field, "state="):
				candidate = State(strings.TrimPrefix(field, "state="))
			case strings.HasPrefix(field, "cycle="):
				candidateCycle, _ = strconv.Atoi(strings.TrimPrefix(field, "cycle="))
			}
		}
		if candidateCycle > cycle || (candidateCycle == cycle && comment.ID > id) {
			state, cycle, id = candidate, candidateCycle, comment.ID
		}
	}
	return state, cycle, id
}

func consumedForCycle(comments []Comment, cycle int) int64 {
	for _, comment := range comments {
		if !strings.HasPrefix(comment.Body, "<!-- gc-queue-system -->\ngc-queue-response-consumed-v3 ") {
			continue
		}
		fields := strings.Fields(comment.Body)
		matchesCycle := false
		var response int64
		for _, field := range fields {
			switch {
			case field == fmt.Sprintf("cycle=%d", cycle):
				matchesCycle = true
			case strings.HasPrefix(field, "response_id="):
				response, _ = strconv.ParseInt(strings.TrimPrefix(field, "response_id="), 10, 64)
			}
		}
		if matchesCycle && response > 0 {
			return response
		}
	}
	return 0
}

func qualifyingResponse(comments []Comment, boundary int64) (Comment, bool) {
	for _, comment := range comments {
		if comment.ID <= boundary || strings.Contains(comment.Body, "<!-- gc-queue-system -->") {
			continue
		}
		return comment, true
	}
	return Comment{}, false
}
