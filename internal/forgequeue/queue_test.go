package forgequeue

import (
	"context"
	"errors"
	"os"
	"sort"
	"sync"
	"testing"
	"time"
)

type fakeForge struct {
	mu             sync.Mutex
	issue          Issue
	comments       []Comment
	labels         []Label
	issuePages     [][]Issue
	commentFailure error
	setFailures    int
	setPartial     bool
	commentAmbig   bool
	nextComment    int64
}

func (f *fakeForge) ListIssues(_ context.Context, page, _ int) ([]Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.issuePages) != 0 {
		if page > len(f.issuePages) {
			return nil, nil
		}
		return append([]Issue(nil), f.issuePages[page-1]...), nil
	}
	if page == 1 {
		return []Issue{f.issue}, nil
	}
	return nil, nil
}

func (f *fakeForge) GetIssue(_ context.Context, _ int) (Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issue, nil
}

func (f *fakeForge) ListComments(_ context.Context, _ int, page, limit int) ([]Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sorted := append([]Comment(nil), f.comments...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	start := (page - 1) * limit
	if start >= len(sorted) {
		return nil, nil
	}
	end := start + limit
	if end > len(sorted) {
		end = len(sorted)
	}
	return sorted[start:end], nil
}

func (f *fakeForge) ListLabels(_ context.Context, page, limit int) ([]Label, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	start := (page - 1) * limit
	if start >= len(f.labels) {
		return nil, nil
	}
	end := start + limit
	if end > len(f.labels) {
		end = len(f.labels)
	}
	return append([]Label(nil), f.labels[start:end]...), nil
}

func (f *fakeForge) CreateLabel(_ context.Context, name, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labels = append(f.labels, Label{ID: int64(len(f.labels) + 1), Name: name})
	return nil
}

func (f *fakeForge) SetIssueLabels(_ context.Context, _ int, desired State, labels []Label) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := make([]Label, 0, len(labels)+1)
	for _, label := range labels {
		if !State(label.Name).Valid() {
			kept = append(kept, label)
		}
	}
	if f.setPartial {
		f.issue.Labels = kept
		f.setPartial = false
	}
	if f.setFailures > 0 {
		f.setFailures--
		return errors.New("injected label transition failure")
	}
	f.issue.Labels = append(kept, Label{Name: string(desired)})
	return nil
}

func (f *fakeForge) CreateComment(_ context.Context, _ int, body string) (Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextComment++
	comment := Comment{ID: f.nextComment, Body: body, CreatedAt: time.Unix(1700000000, 0)}
	f.comments = append(f.comments, comment)
	if f.commentFailure != nil {
		err := f.commentFailure
		f.commentFailure = nil
		return Comment{}, err
	}
	if f.commentAmbig {
		f.commentAmbig = false
		return Comment{}, errors.New("response lost after accepted comment")
	}
	return comment, nil
}

type fakeLauncher struct {
	mu    sync.Mutex
	items []WorkItem
	err   error
}

func (l *fakeLauncher) Launch(_ context.Context, item WorkItem) (LaunchResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return LaunchResult{}, l.err
	}
	l.items = append(l.items, item)
	return LaunchResult{WorkflowID: "wf-" + string(rune('0'+len(l.items))), BeadID: "bead"}, nil
}

func testQueue(t *testing.T, forge *fakeForge, launcher *fakeLauncher, pageSize int) *Queue {
	t.Helper()
	stateDir := t.TempDir()
	q, err := New(Config{Forge: "github", Repository: "fixture/repo", AllowedRepositories: []string{"fixture/repo"}, TargetBranch: "main", StateDir: stateDir, PageSize: pageSize, MaxPages: 20, Now: func() time.Time { return time.Unix(1700000000, 0) }}, forge, launcher)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func identifiedIssue(state State) Issue {
	return Issue{Number: 7, Title: "queue work", Body: "<!-- gc-queue-v3 forge=github repository=fixture/repo -->\nDo the work", Labels: []Label{{Name: string(state)}}}
}

func allQueueLabels() []Label {
	return []Label{{ID: 1, Name: "gc:queued"}, {ID: 2, Name: "gc:working"}, {ID: 3, Name: "gc:needs-human"}, {ID: 4, Name: "gc:blocked"}, {ID: 5, Name: "gc:done"}}
}

func TestPollClaimsExactlyOnceAndUsesPaginatedIssueAndLabelReads(t *testing.T) {
	forge := &fakeForge{issue: identifiedIssue(StateQueued), labels: allQueueLabels(), issuePages: [][]Issue{{{Number: 1, Body: "unrelated"}, {Number: 2, Body: "unrelated"}}, {identifiedIssue(StateQueued)}}}
	launcher := &fakeLauncher{}
	q := testQueue(t, forge, launcher, 2)
	first, err := q.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Action != "launched" || len(launcher.items) != 1 {
		t.Fatalf("first poll = %#v launches=%d, want one launch", first, len(launcher.items))
	}
	second, err := q.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Action != "idle" || len(launcher.items) != 1 {
		t.Fatalf("second poll = %#v launches=%d, want idle and no duplicate", second, len(launcher.items))
	}
}

func TestHumanCyclesUseExactCommentIDsAndAllowSameSecondResponses(t *testing.T) {
	forge := &fakeForge{issue: identifiedIssue(StateQueued), labels: allQueueLabels()}
	launcher := &fakeLauncher{}
	q := testQueue(t, forge, launcher, 2)
	if _, err := q.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := q.Ask(context.Background(), 7, "first question"); err != nil {
		t.Fatal(err)
	}
	forge.mu.Lock()
	forge.nextComment++
	forge.comments = append(forge.comments, Comment{ID: forge.nextComment, Body: "old answer", CreatedAt: time.Unix(1700000000, 0)})
	forge.mu.Unlock()
	first, err := q.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Action != "launched" || first.ResponseID == 0 {
		t.Fatalf("first resume = %#v, want exact response", first)
	}
	if err := q.Ask(context.Background(), 7, "second question"); err != nil {
		t.Fatal(err)
	}
	forge.mu.Lock()
	forge.nextComment++
	forge.comments = append(forge.comments, Comment{ID: forge.nextComment, Body: "new answer", CreatedAt: time.Unix(1700000000, 0)})
	forge.mu.Unlock()
	second, err := q.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Action != "launched" || second.ResponseID == first.ResponseID || len(launcher.items) != 3 {
		t.Fatalf("second resume = %#v launches=%d, want distinct response and third total launch", second, len(launcher.items))
	}
	third, err := q.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if third.Action != "idle" || len(launcher.items) != 3 {
		t.Fatalf("post-resume poll = %#v launches=%d, want no duplicate", third, len(launcher.items))
	}
}

func TestResponseBeyondFirstCommentPageAndTransitionRecovery(t *testing.T) {
	forge := &fakeForge{issue: identifiedIssue(StateWorking), labels: allQueueLabels()}
	launcher := &fakeLauncher{}
	q := testQueue(t, forge, launcher, 2)
	if err := q.Ask(context.Background(), 7, "question"); err != nil {
		t.Fatal(err)
	}
	forge.mu.Lock()
	for i := 0; i < 3; i++ {
		forge.nextComment++
		forge.comments = append(forge.comments, Comment{ID: forge.nextComment, Body: "noise", CreatedAt: time.Unix(1700000000, 0)})
	}
	forge.nextComment++
	forge.comments = append(forge.comments, Comment{ID: forge.nextComment, Body: "response beyond page one", CreatedAt: time.Unix(1700000000, 0)})
	forge.mu.Unlock()
	forge.setPartial = true
	forge.setFailures = 1
	result, err := q.PollOnce(context.Background())
	if err == nil {
		t.Fatal("injected transition failure was not surfaced")
	}
	if result.Action != "" {
		t.Fatalf("failed poll result = %#v, want no action", result)
	}
	if _, err := q.PollOnce(context.Background()); err != nil {
		t.Fatal("recovery poll failed:", err)
	}
	if len(launcher.items) != 1 {
		t.Fatalf("recovery launches=%d, want one", len(launcher.items))
	}
}

func TestAmbiguousCommentIsReadBackWithoutDuplicate(t *testing.T) {
	forge := &fakeForge{issue: identifiedIssue(StateWorking), labels: allQueueLabels(), commentAmbig: true}
	launcher := &fakeLauncher{}
	q := testQueue(t, forge, launcher, 4)
	if err := q.Ask(context.Background(), 7, "question"); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, comment := range forge.comments {
		if comment.Body == "question" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("question comment count=%d, want one", count)
	}
}

func TestConcurrentPollReturnsBusyAndDoesNotDuplicate(t *testing.T) {
	forge := &fakeForge{issue: identifiedIssue(StateQueued), labels: allQueueLabels()}
	launcher := &fakeLauncher{}
	q := testQueue(t, forge, launcher, 4)
	if err := q.lockOnce(); err != nil {
		t.Fatal(err)
	}
	defer q.unlock()
	result, err := q.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != "busy" {
		t.Fatalf("poll while locked = %#v, want busy", result)
	}
}

func TestRecoveryAfterLocalStateLossUsesForgeLaunchResultWithoutDuplicate(t *testing.T) {
	forge := &fakeForge{issue: identifiedIssue(StateQueued), labels: allQueueLabels()}
	launcher := &fakeLauncher{}
	q := testQueue(t, forge, launcher, 4)

	first, err := q.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Action != "launched" || len(launcher.items) != 1 {
		t.Fatalf("initial poll = %#v launches=%d, want one launch", first, len(launcher.items))
	}
	if err := os.Remove(q.statePath(7)); err != nil {
		t.Fatal(err)
	}

	recovered, err := q.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Action != "reconciled" || recovered.WorkflowID != first.WorkflowID || len(launcher.items) != 1 {
		t.Fatalf("recovery poll = %#v launches=%d, want forge result and no duplicate", recovered, len(launcher.items))
	}
	third, err := q.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if third.Action != "idle" || len(launcher.items) != 1 {
		t.Fatalf("post-recovery poll = %#v launches=%d, want idle and no duplicate", third, len(launcher.items))
	}
}

func TestRecoveryAfterDurableLaunchIntentBlocksAmbiguousExecution(t *testing.T) {
	forge := &fakeForge{issue: identifiedIssue(StateWorking), labels: allQueueLabels()}
	launcher := &fakeLauncher{}
	q := testQueue(t, forge, launcher, 4)
	state := q.initState(7, StateWorking, "claim-crash", 0)
	state.LaunchStatus = "starting"
	if err := q.writeState(state); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		"gc-queue-claim-v3 claim_id=claim-crash",
		"gc-queue-state-intent-v3 state=gc:working cycle=0",
		launchIntentBody("claim-crash", 0, 0),
	} {
		if _, err := q.systemComment(context.Background(), 7, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(q.statePath(7)); err != nil {
		t.Fatal(err)
	}

	result, err := q.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != "blocked" || result.Reason != "launch_interrupted" || len(launcher.items) != 0 {
		t.Fatalf("ambiguous recovery = %#v launches=%d, want explicit block and no retry", result, len(launcher.items))
	}
	if got, err := stateOf(forge.issue); err != nil || got != StateBlocked {
		t.Fatalf("forge state = %q, err=%v, want blocked", got, err)
	}
	recovered, err := q.readState(7)
	if err != nil {
		t.Fatal(err)
	}
	if recovered == nil || recovered.LastError != "launch_interrupted" {
		t.Fatalf("recovered state = %#v, want machine-readable launch_interrupted", recovered)
	}
}

func TestRecoveryBeforeLaunchIntentRelaunchesRecoverableWorkOnce(t *testing.T) {
	forge := &fakeForge{issue: identifiedIssue(StateWorking), labels: allQueueLabels()}
	launcher := &fakeLauncher{}
	q := testQueue(t, forge, launcher, 4)
	state := q.initState(7, StateWorking, "claim-before-launch", 0)
	if err := q.writeState(state); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		"gc-queue-claim-v3 claim_id=claim-before-launch",
		"gc-queue-state-intent-v3 state=gc:working cycle=0",
	} {
		if _, err := q.systemComment(context.Background(), 7, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(q.statePath(7)); err != nil {
		t.Fatal(err)
	}

	result, err := q.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != "launched" || len(launcher.items) != 1 {
		t.Fatalf("pre-launch recovery = %#v launches=%d, want one recovered launch", result, len(launcher.items))
	}
}

func TestLauncherFailurePersistsBlockedMachineState(t *testing.T) {
	forge := &fakeForge{issue: identifiedIssue(StateQueued), labels: allQueueLabels()}
	launcher := &fakeLauncher{err: errors.New("launcher unavailable")}
	q := testQueue(t, forge, launcher, 4)

	result, err := q.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != "blocked" || result.Reason != "launcher_failed" {
		t.Fatalf("launcher failure result = %#v, want blocked launcher_failed", result)
	}
	recovered, err := q.readState(7)
	if err != nil {
		t.Fatal(err)
	}
	if recovered == nil || recovered.State != StateBlocked || recovered.LastError != "launcher_failed" || recovered.LaunchStatus != "failed" {
		t.Fatalf("launcher failure state = %#v, want blocked machine-readable failure", recovered)
	}
}

func TestNeedsHumanResumeSurvivesStateLossAndResumesExactlyOnce(t *testing.T) {
	forge := &fakeForge{issue: identifiedIssue(StateQueued), labels: allQueueLabels()}
	launcher := &fakeLauncher{}
	q := testQueue(t, forge, launcher, 4)
	if _, err := q.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := q.Ask(context.Background(), 7, "please choose a deployment target"); err != nil {
		t.Fatal(err)
	}
	forge.mu.Lock()
	forge.nextComment++
	forge.comments = append(forge.comments, Comment{ID: forge.nextComment, Body: "resume now", CreatedAt: time.Unix(1700000000, 0)})
	forge.mu.Unlock()
	if err := os.Remove(q.statePath(7)); err != nil {
		t.Fatal(err)
	}

	resumed, err := q.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Action != "launched" || resumed.ResponseID == 0 || len(launcher.items) != 2 {
		t.Fatalf("resume after state loss = %#v launches=%d, want one resume", resumed, len(launcher.items))
	}
	if err := os.Remove(q.statePath(7)); err != nil {
		t.Fatal(err)
	}
	reconciled, err := q.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Action != "reconciled" || reconciled.ResponseID != resumed.ResponseID || len(launcher.items) != 2 {
		t.Fatalf("resume reconciliation = %#v launches=%d, want exact-once resume", reconciled, len(launcher.items))
	}
}

func TestTerminalDoneDoesNotRelaunchAfterStateLoss(t *testing.T) {
	forge := &fakeForge{issue: identifiedIssue(StateQueued), labels: allQueueLabels()}
	launcher := &fakeLauncher{}
	q := testQueue(t, forge, launcher, 4)
	if _, err := q.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := q.Transition(context.Background(), 7, StateDone); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(q.statePath(7)); err != nil {
		t.Fatal(err)
	}
	result, err := q.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != "idle" || len(launcher.items) != 1 {
		t.Fatalf("done recovery = %#v launches=%d, want terminal idle", result, len(launcher.items))
	}
}
