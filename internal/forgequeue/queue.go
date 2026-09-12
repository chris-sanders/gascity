package forgequeue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

func (q *Queue) PollOnce(ctx context.Context) (PollResult, error) {
	if err := q.lockOnce(); err != nil {
		if errors.Is(err, ErrBusy) {
			return q.result("busy", 0, ""), nil
		}
		return PollResult{}, err
	}
	defer q.unlock()

	if err := q.ensureLabels(ctx); err != nil {
		return PollResult{}, err
	}
	issues, err := q.issues(ctx)
	if err != nil {
		return PollResult{}, err
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].ID() < issues[j].ID() })
	for _, listed := range issues {
		if listed.ID() == 0 || !strings.Contains(listed.Body, q.Config.Marker) || listed.PullRequest != nil {
			continue
		}
		issue, err := q.Forge.GetIssue(ctx, listed.ID())
		if err != nil {
			return PollResult{}, err
		}
		if !strings.Contains(issue.Body, q.Config.Marker) {
			return PollResult{}, fmt.Errorf("issue %d lacks explicit forge/repository identity", issue.ID())
		}
		if action, handled, err := q.reconcile(ctx, issue); err != nil {
			return PollResult{}, err
		} else if handled {
			return action, nil
		}
		state, err := stateOf(issue)
		if err != nil {
			return PollResult{}, err
		}
		switch state {
		case StateQueued:
			return q.claimAndLaunch(ctx, issue)
		case StateNeedsHuman:
			if result, resumed, err := q.resumeIfAnswered(ctx, issue); err != nil {
				return PollResult{}, err
			} else if resumed {
				return result, nil
			}
		case StateWorking, StateBlocked, StateDone:
			// Working is owned by a previously launched workflow. Blocked and
			// done are terminal until an operator explicitly relabels them.
		case "":
			return PollResult{}, fmt.Errorf("issue %d has no queue state label", issue.ID())
		}
	}
	return q.result("idle", 0, ""), nil
}

func (q *Queue) result(action string, issue int, state State) PollResult {
	return PollResult{Action: action, Forge: q.Config.Forge, Repository: q.Config.Repository, Issue: issue, State: state}
}

func (q *Queue) reconcile(ctx context.Context, issue Issue) (PollResult, bool, error) {
	comments, err := q.comments(ctx, issue.ID())
	if err != nil {
		return PollResult{}, false, err
	}
	intentState, intentCycle, _ := latestStateIntent(comments)
	current, err := stateOf(issue)
	if err != nil {
		return PollResult{}, false, err
	}
	if intentState == "" {
		if current == "" {
			return PollResult{}, false, fmt.Errorf("issue %d has no durable queue state or intent", issue.ID())
		}
		return PollResult{}, false, nil
	}
	desired := intentState
	if !desired.Valid() {
		return PollResult{}, false, fmt.Errorf("issue %d has invalid durable queue state %q", issue.ID(), desired)
	}
	state, err := q.readState(issue.ID())
	if err != nil {
		return PollResult{}, false, err
	}
	if state == nil {
		newState := q.initState(issue.ID(), desired, "recovered-"+strconv.Itoa(issue.ID()), intentCycle)
		if err := q.writeState(newState); err != nil {
			return PollResult{}, false, err
		}
		state = &newState
	}
	if state.HumanCycle < intentCycle {
		state.HumanCycle = intentCycle
		if err := q.writeState(*state); err != nil {
			return PollResult{}, false, err
		}
	}
	if current == desired {
		if desired == StateWorking && state.LaunchStatus == "pending" {
			result, err := q.launch(ctx, issue, *state, 0, "")
			return result, true, err
		}
		if desired == StateWorking && state.LaunchStatus == "starting" {
			state.LastError = "launch_interrupted"
			state.State = StateBlocked
			if err := q.writeState(*state); err != nil {
				return PollResult{}, false, err
			}
			if err := q.transition(ctx, issue.ID(), StateBlocked, "launch interrupted after durable intent"); err != nil {
				return PollResult{}, false, err
			}
			return PollResult{}, true, nil
		}
		return PollResult{}, false, nil
	}
	if current == StateNeedsHuman && desired == StateWorking {
		// A consumed response marker is the durable proof that a resume was
		// intended. The normal resume path handles the label mutation and launch.
		if consumed := consumedForCycle(comments, state.HumanCycle); consumed > 0 {
			response, ok := findComment(comments, consumed)
			if !ok {
				return PollResult{}, false, fmt.Errorf("consumed response %d is not readable", consumed)
			}
			result, err := q.launchAfterResume(ctx, issue, *state, response)
			return result, true, err
		}
	}
	if err := q.setStateWithIntent(ctx, issue.ID(), desired, intentCycle); err != nil {
		return PollResult{}, false, err
	}
	state.State = desired
	if desired == StateNeedsHuman {
		state.HumanCycle = intentCycle
	}
	if err := q.writeState(*state); err != nil {
		return PollResult{}, false, err
	}
	if desired == StateWorking && state.LaunchStatus == "pending" {
		result, err := q.launch(ctx, issue, *state, 0, "")
		return result, true, err
	}
	return PollResult{}, false, nil
}

func (q *Queue) claimAndLaunch(ctx context.Context, issue Issue) (PollResult, error) {
	current, err := stateOf(issue)
	if err != nil {
		return PollResult{}, err
	}
	if current != StateQueued {
		return q.result("idle", issue.ID(), current), nil
	}
	state, err := q.readState(issue.ID())
	if err != nil {
		return PollResult{}, err
	}
	claimID := "claim-" + q.Config.Forge + "-" + strconv.Itoa(issue.ID()) + "-" + strconv.FormatInt(q.Config.Now().UnixNano(), 10)
	if state == nil {
		newState := q.initState(issue.ID(), StateWorking, claimID, 0)
		state = &newState
	} else {
		state.State = StateWorking
		if state.ClaimID == "" {
			state.ClaimID = claimID
		}
	}
	state.LaunchStatus = "pending"
	if err := q.writeState(*state); err != nil {
		return PollResult{}, err
	}
	if _, err := q.systemComment(ctx, issue.ID(), "gc-queue-claim-v3 claim_id="+state.ClaimID); err != nil {
		return PollResult{}, err
	}
	if err := q.setStateWithIntent(ctx, issue.ID(), StateWorking, 0); err != nil {
		return PollResult{}, err
	}
	return q.launch(ctx, issue, *state, 0, "")
}

func (q *Queue) launch(ctx context.Context, issue Issue, state itemState, responseID int64, responseBody string) (PollResult, error) {
	if state.LaunchStatus == "launched" {
		result := q.result("reconciled", issue.ID(), StateWorking)
		result.WorkflowID, result.BeadID = state.WorkflowID, state.BeadID
		return result, nil
	}
	if _, err := q.systemComment(ctx, issue.ID(), fmt.Sprintf("gc-queue-launch-intent-v3 cycle=%d response_id=%s", state.HumanCycle, int64String(responseID))); err != nil {
		return PollResult{}, err
	}
	state.LaunchStatus = "starting"
	if err := q.writeState(state); err != nil {
		return PollResult{}, err
	}
	result, err := q.Launcher.Launch(ctx, WorkItem{Forge: q.Config.Forge, Repository: q.Config.Repository, GiteaBaseURL: q.Config.GiteaBaseURL, Issue: issue, TargetBranch: q.Config.TargetBranch, ResponseCommentID: responseID, ResponseCommentBody: responseBody})
	if err != nil {
		state.LaunchStatus = "failed"
		state.LastError = "launcher_failed"
		_ = q.writeState(state)
		if transitionErr := q.transition(ctx, issue.ID(), StateBlocked, "queue launch failed; safe to inspect and explicitly requeue"); transitionErr != nil {
			return PollResult{}, fmt.Errorf("launch failed: %v; blocking transition failed: %w", err, transitionErr)
		}
		return PollResult{Action: "blocked", Forge: q.Config.Forge, Repository: q.Config.Repository, Issue: issue.ID(), State: StateBlocked, Reason: "launcher_failed"}, nil
	}
	state.LaunchStatus = "launched"
	state.WorkflowID = result.WorkflowID
	state.BeadID = result.BeadID
	if err := q.writeState(state); err != nil {
		return PollResult{}, err
	}
	return PollResult{Action: "launched", Forge: q.Config.Forge, Repository: q.Config.Repository, Issue: issue.ID(), State: StateWorking, WorkflowID: result.WorkflowID, BeadID: result.BeadID, ResponseID: responseID}, nil
}

func (q *Queue) resumeIfAnswered(ctx context.Context, issue Issue) (PollResult, bool, error) {
	comments, err := q.comments(ctx, issue.ID())
	if err != nil {
		return PollResult{}, false, err
	}
	cycle, boundary := latestCycleMarker(comments, "gc-queue-needs-human-boundary-v3 ")
	if cycle == 0 || boundary == 0 {
		return PollResult{}, false, nil
	}
	state, err := q.readState(issue.ID())
	if err != nil {
		return PollResult{}, false, err
	}
	if state == nil {
		newState := q.initState(issue.ID(), StateNeedsHuman, "recovered-needs-human-"+strconv.Itoa(issue.ID()), cycle)
		state = &newState
	}
	state.HumanCycle = cycle
	state.BoundaryCommentID = boundary
	if consumed := consumedForCycle(comments, cycle); consumed > 0 {
		response, ok := findComment(comments, consumed)
		if !ok {
			return PollResult{}, false, fmt.Errorf("consumed response %d is not readable", consumed)
		}
		if state.LaunchStatus == "launched" {
			return q.result("idle", issue.ID(), StateNeedsHuman), false, q.writeState(*state)
		}
		result, err := q.launchAfterResume(ctx, issue, *state, response)
		return result, true, err
	}
	response, ok := qualifyingResponse(sortedComments(comments), boundary)
	if !ok {
		return PollResult{}, false, q.writeState(*state)
	}
	if _, err := q.systemComment(ctx, issue.ID(), fmt.Sprintf("gc-queue-response-consumed-v3 cycle=%d response_id=%d", cycle, response.ID)); err != nil {
		return PollResult{}, false, err
	}
	state.ConsumedResponses = appendUniqueConsumed(state.ConsumedResponses, Consumed{Cycle: cycle, CommentID: response.ID})
	state.LaunchStatus = "pending"
	if err := q.writeState(*state); err != nil {
		return PollResult{}, false, err
	}
	result, err := q.launchAfterResume(ctx, issue, *state, response)
	return result, true, err
}

func (q *Queue) launchAfterResume(ctx context.Context, issue Issue, state itemState, response Comment) (PollResult, error) {
	if err := q.setStateWithIntent(ctx, issue.ID(), StateWorking, state.HumanCycle); err != nil {
		return PollResult{}, err
	}
	state.State = StateWorking
	return q.launch(ctx, issue, state, response.ID, response.Body)
}

func (q *Queue) transition(ctx context.Context, issue int, desired State, reason string) error {
	if !desired.Valid() {
		return fmt.Errorf("invalid queue state %q", desired)
	}
	value, err := q.Forge.GetIssue(ctx, issue)
	if err != nil {
		return err
	}
	current, err := stateOf(value)
	if err != nil {
		return err
	}
	if current == desired {
		return nil
	}
	if !validTransition(current, desired) {
		return fmt.Errorf("invalid queue transition %s -> %s", current, desired)
	}
	cycle := 0
	if state, readErr := q.readState(issue); readErr == nil && state != nil {
		cycle = state.HumanCycle
	}
	if desired == StateNeedsHuman {
		cycle++
	}
	return q.setStateWithIntent(ctx, issue, desired, cycle, reason)
}

func validTransition(from, to State) bool {
	switch string(from) + "->" + string(to) {
	case string(StateQueued) + "->" + string(StateWorking),
		string(StateWorking) + "->" + string(StateNeedsHuman),
		string(StateNeedsHuman) + "->" + string(StateWorking),
		string(StateWorking) + "->" + string(StateDone),
		string(StateWorking) + "->" + string(StateBlocked),
		string(StateBlocked) + "->" + string(StateQueued),
		string(StateDone) + "->" + string(StateQueued):
		return true
	default:
		return false
	}
}

func (q *Queue) setStateWithIntent(ctx context.Context, issue int, desired State, cycle int, reasons ...string) error {
	body := fmt.Sprintf("gc-queue-state-intent-v3 state=%s cycle=%d", desired, cycle)
	if len(reasons) > 0 && strings.TrimSpace(reasons[0]) != "" {
		body += " reason=" + strings.ReplaceAll(strings.TrimSpace(reasons[0]), "\n", " ")
	}
	if _, err := q.systemComment(ctx, issue, body); err != nil {
		return err
	}
	if desired == StateNeedsHuman {
		comment, err := q.systemComment(ctx, issue, fmt.Sprintf("gc-queue-needs-human-boundary-v3 cycle=%d", cycle))
		if err != nil {
			return err
		}
		if state, readErr := q.readState(issue); readErr == nil && state != nil {
			state.HumanCycle, state.BoundaryCommentID = cycle, comment.ID
			if err := q.writeState(*state); err != nil {
				return err
			}
		}
	}
	value, err := q.Forge.GetIssue(ctx, issue)
	if err != nil {
		return err
	}
	if err := q.Forge.SetIssueLabels(ctx, issue, desired, value.Labels); err != nil {
		return err
	}
	updated, err := q.Forge.GetIssue(ctx, issue)
	if err != nil {
		return err
	}
	state, err := stateOf(updated)
	if err != nil {
		return err
	}
	if state != desired {
		return fmt.Errorf("state transition did not durably reconcile to %s", desired)
	}
	return nil
}

func (q *Queue) Ask(ctx context.Context, issue int, question string) error {
	question = strings.TrimSpace(question)
	if question == "" {
		return fmt.Errorf("question is required")
	}
	if _, err := q.createComment(ctx, issue, question); err != nil {
		return err
	}
	if err := q.transition(ctx, issue, StateNeedsHuman, "human response required"); err != nil {
		return err
	}
	return nil
}

func (q *Queue) Transition(ctx context.Context, issue int, desired State) error {
	return q.transition(ctx, issue, desired, "")
}

func (q *Queue) Show(ctx context.Context, issue int) (Issue, error) {
	value, err := q.Forge.GetIssue(ctx, issue)
	if err != nil {
		return Issue{}, err
	}
	if !strings.Contains(value.Body, q.Config.Marker) {
		return Issue{}, fmt.Errorf("issue %d lacks explicit forge/repository identity", issue)
	}
	return value, nil
}

func (q *Queue) systemComment(ctx context.Context, issue int, body string) (Comment, error) {
	exact := q.markerBody(body)
	comments, err := q.comments(ctx, issue)
	if err != nil {
		return Comment{}, err
	}
	if id := systemCommentID(comments, exact); id > 0 {
		comment, _ := findComment(comments, id)
		return comment, nil
	}
	comment, err := q.createComment(ctx, issue, exact)
	if err != nil {
		return Comment{}, err
	}
	return comment, nil
}

func (q *Queue) createComment(ctx context.Context, issue int, body string) (Comment, error) {
	comment, err := q.Forge.CreateComment(ctx, issue, body)
	if err != nil {
		// A Forge accepted a POST while the response was lost. Read back the
		// exact marker before allowing a safe retry.
		comments, readErr := q.comments(ctx, issue)
		if readErr == nil {
			if id := systemCommentID(comments, body); id > 0 {
				found, _ := findComment(comments, id)
				return found, nil
			}
		}
		return Comment{}, err
	}
	if comment.ID <= 0 {
		return Comment{}, fmt.Errorf("forge comment has no durable identity")
	}
	return comment, nil
}

func sortedComments(comments []Comment) []Comment {
	result := append([]Comment(nil), comments...)
	sort.SliceStable(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func findComment(comments []Comment, id int64) (Comment, bool) {
	for _, comment := range comments {
		if comment.ID == id {
			return comment, true
		}
	}
	return Comment{}, false
}

func appendUniqueConsumed(values []Consumed, value Consumed) []Consumed {
	for _, existing := range values {
		if existing.Cycle == value.Cycle && existing.CommentID == value.CommentID {
			return values
		}
	}
	return append(values, value)
}

func int64String(value int64) string {
	if value == 0 {
		return "none"
	}
	return strconv.FormatInt(value, 10)
}

func marshalIndent(value any) ([]byte, error) {
	return json.MarshalIndent(value, "", "  ")
}

func unmarshalStrict(data []byte, target any) error {
	return json.Unmarshal(data, target)
}

// ErrorState persists a machine-readable retry-safe poll failure without
// putting credentials or response bodies in the state directory.
func (q *Queue) ErrorState(err error) error {
	value := struct {
		Schema     string `json:"schema"`
		Status     string `json:"status"`
		Forge      string `json:"forge"`
		Repository string `json:"repository"`
		At         int64  `json:"at"`
		Reason     string `json:"reason"`
	}{Schema: "gc-queue-error-v1", Status: "retry-safe", Forge: q.Config.Forge, Repository: q.Config.Repository, At: q.Config.Now().Unix(), Reason: "deterministic-poll-failure"}
	data, marshalErr := json.MarshalIndent(value, "", "  ")
	if marshalErr != nil {
		return marshalErr
	}
	path := q.Config.StateDir + "/last-error.json"
	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		return writeErr
	}
	return err
}
