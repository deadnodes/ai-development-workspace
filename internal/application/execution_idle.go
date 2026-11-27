package application

import (
	"context"
	"fmt"
	"slices"
	"time"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

var groupIdleTypes = [...]string{domain.EventGroupIdle, domain.EventGroupWaitTimeout, domain.EventGroupMemberActionable, domain.EventGroupMemberStale, domain.EventGroupOwnerBlocked}

func groupIdleOwnerVisible(ctx context.Context, owner string) bool {
	_, authenticated := auth.FromContext(ctx)
	return !authenticated || owner != "" && owner == workUser(ctx)
}

func groupIdleEffectiveAttempts(event domain.ExecutionEvent) int {
	attempts := event.Delivery.Attempts
	if domain.IsGroupIdleEvent(event.Type) {
		for _, ack := range event.Acknowledgements {
			if ack.Outcome == "skipped_active" || ack.Outcome == "delivered" {
				attempts--
			}
		}
	}
	return attempts
}

type groupIdleCheck struct {
	Armed     bool       `json:"armed"`
	NextDueAt *time.Time `json:"next_due_at,omitempty"`
}

type groupIdleEvaluation struct {
	GroupActivityAt time.Time                  `json:"group_activity_at"`
	Pending         int                        `json:"pending"`
	OwnerBlocked    int                        `json:"owner_blocked"`
	Policy          domain.ExecutionIdlePolicy `json:"policy"`
	Checks          map[string]groupIdleCheck  `json:"checks"`
	NextDueAt       *time.Time                 `json:"next_due_at,omitempty"`
	staleMembers    []domain.ExecutionGroupIdleMember
}

func idlePolicy(st *domain.State, productID string) domain.ExecutionIdlePolicy {
	if product := productByID(*st, productID); product != nil && product.IdlePolicy != nil {
		return *product.IdlePolicy
	}
	return domain.ExecutionIdlePolicy{IdleAfterSeconds: 600, WaitTimeoutSeconds: 1800, OwnerBlockedIntervalSeconds: 14400, MemberStaleAfterSeconds: 900}
}

func setProductIdlePolicy(ctx context.Context, product *domain.Product, data map[string]any) error {
	if principal, ok := auth.FromContext(ctx); ok && !instanceAdmin(principal) && (principal.Kind != "human" || principal.ID != product.Owner) {
		return auth.ErrForbidden
	}
	var input struct {
		IdleAfterSeconds            *int `json:"idle_after_seconds"`
		WaitTimeoutSeconds          *int `json:"wait_timeout_seconds"`
		OwnerBlockedIntervalSeconds *int `json:"owner_blocked_interval_seconds"`
		MemberStaleAfterSeconds     *int `json:"member_stale_after_seconds"`
	}
	if err := decode(data, &input); err != nil {
		return err
	}
	for _, value := range []*int{input.IdleAfterSeconds, input.WaitTimeoutSeconds, input.OwnerBlockedIntervalSeconds, input.MemberStaleAfterSeconds} {
		if value == nil || *value < 0 || *value > domain.MaxExecutionIdleSeconds {
			return invalid("all idle policy thresholds must be integers between 0 and %d seconds", domain.MaxExecutionIdleSeconds)
		}
	}
	product.IdlePolicy = &domain.ExecutionIdlePolicy{IdleAfterSeconds: *input.IdleAfterSeconds, WaitTimeoutSeconds: *input.WaitTimeoutSeconds, OwnerBlockedIntervalSeconds: *input.OwnerBlockedIntervalSeconds, MemberStaleAfterSeconds: *input.MemberStaleAfterSeconds}
	return nil
}

func laterActivity(current time.Time, observed *time.Time) time.Time {
	if observed != nil && observed.After(current) {
		return *observed
	}
	return current
}

func activityAge(now time.Time, observed *time.Time) *int64 {
	if observed == nil {
		return nil
	}
	seconds := int64(max(0, now.Sub(*observed)/time.Second))
	return &seconds
}

func ownerBlocksGroupMember(st *domain.State, child *domain.Integration, execution *domain.IntegrationExecution) bool {
	humanQuestion := false
	for _, memory := range st.Memories {
		if !executionBlockerInScope(memory, child) {
			continue
		}
		addressedQuestion := questionBlocked(memory) && humanWorkID(memory.Question.AddressedTo)
		if execution != nil && execution.State == "blocked" && execution.BlockerRef == memory.ID && memory.Status == "open" {
			return addressedQuestion || memory.Kind == "blocker" && humanWorkID(memory.Owner)
		}
		humanQuestion = humanQuestion || addressedQuestion
	}
	return humanQuestion
}

func evaluateGroupIdle(st *domain.State, group domain.ExecutionGroup, now time.Time) (groupIdleEvaluation, []domain.ExecutionGroupIdleMember) {
	activity := laterActivity(group.CreatedAt, group.HarnessChangedAt)
	activity = laterActivity(activity, group.TurnEndAt)
	activity = laterActivity(activity, group.TurnStartAt)
	for _, revision := range group.Revisions {
		activity = laterActivity(activity, &revision.CreatedAt)
	}
	result := groupIdleEvaluation{Policy: idlePolicy(st, group.ProductID), Checks: make(map[string]groupIdleCheck, len(groupIdleTypes))}
	staleCheck := groupIdleCheck{}
	var members []domain.ExecutionGroupIdleMember
	actionable := false
	ended := group.TurnEndAt != nil && (group.TurnStartAt == nil || group.TurnEndAt.After(*group.TurnStartAt))
	for _, integrationID := range group.MemberIntegrationIDs {
		child := integration(st, integrationID)
		if !groupOwnsIntegration(group, child) || firstGroupWake(st, child).GroupID != group.ID {
			continue
		}
		execution := executionFor(st, integrationID)
		desired, state := "ready", "unassigned"
		var lastProgress, lastHeartbeat *time.Time
		if execution != nil && execution.ProductID == group.ProductID {
			state = execution.State
			if execution.DesiredState != "" {
				desired = execution.DesiredState
			}
			lastProgress, lastHeartbeat = execution.LastProgress, execution.LastHeartbeat
			activity = laterActivity(laterActivity(activity, lastProgress), lastHeartbeat)
		} else {
			execution = nil
		}
		if statusReachedIntegration(child.Status, desired) || child.Status == "released" || child.Status == "cancelled" || child.Status == "superseded" || state == "completed" || state == "paused" || state == "cancelled" {
			continue
		}
		members = append(members, domain.ExecutionGroupIdleMember{IntegrationID: child.ID, FeatureID: child.FeatureID, State: state, DesiredState: desired, SecondsSinceLastProgress: activityAge(now, lastProgress), SecondsSinceHeartbeat: activityAge(now, lastHeartbeat), NextAction: executionNextAction(child, execution, st.ExecutionEvents, &group)["code"]})
		if ownerBlocksGroupMember(st, child, execution) {
			result.OwnerBlocked++
			continue
		}
		result.Pending++
		if result.Policy.MemberStaleAfterSeconds > 0 {
			clock := memberJoinedAt(group, integrationID)
			if execution != nil {
				clock = laterActivity(clock, execution.LastRecordedProgress)
			}
			due := clock.Add(time.Duration(result.Policy.MemberStaleAfterSeconds) * time.Second)
			staleCheck.Armed = true
			if staleCheck.NextDueAt == nil || due.Before(*staleCheck.NextDueAt) {
				staleCheck.NextDueAt = &due
			}
			if !now.Before(due) {
				member := members[len(members)-1]
				member.ProgressClockAt = &clock
				member.SecondsSinceLastProgress = activityAge(now, &clock)
				result.staleMembers = append(result.staleMembers, member)
			}
		}
		if state == "unassigned" || state == "lost" || state == "exited_incomplete" || execution != nil && execution.Reconciliation.ActionRequired || ended && memberAddedAfter(group, integrationID, *group.TurnEndAt) {
			actionable = true
		}
	}
	result.GroupActivityAt = activity
	registered := group.ClosedAt == nil && group.OrchestratorHarness.Kind != "" && (group.OrchestratorHarness.SessionRef != "" || group.OrchestratorHarness.TmuxTarget != "")
	arm := func(eventType string, condition bool, seconds int) {
		check := groupIdleCheck{Armed: registered && condition && seconds > 0}
		if check.Armed {
			due := activity.Add(time.Duration(seconds) * time.Second)
			check.NextDueAt = &due
		}
		result.Checks[eventType] = check
	}
	arm(domain.EventGroupIdle, ended && result.Pending > 0, result.Policy.IdleAfterSeconds)
	arm(domain.EventGroupWaitTimeout, result.Pending > 0, result.Policy.WaitTimeoutSeconds)
	arm(domain.EventGroupMemberActionable, ended && actionable, 60)
	result.Checks[domain.EventGroupMemberStale] = staleCheck
	arm(domain.EventGroupOwnerBlocked, result.Pending == 0 && result.OwnerBlocked > 0, result.Policy.OwnerBlockedIntervalSeconds)
	return result, members
}

func memberAddedAfter(group domain.ExecutionGroup, integrationID string, cutoff time.Time) bool {
	return memberJoinedAt(group, integrationID).After(cutoff)
}

func memberJoinedAt(group domain.ExecutionGroup, integrationID string) time.Time {
	for index := len(group.Revisions) - 1; index >= 0; index-- {
		if !slices.Contains(group.Revisions[index].MemberIntegrationIDs, integrationID) {
			if index+1 < len(group.Revisions) {
				return group.Revisions[index+1].CreatedAt
			}
			break
		}
	}
	return group.CreatedAt
}

func groupIdleSameEpisode(event domain.ExecutionEvent, group domain.ExecutionGroup, evaluation groupIdleEvaluation, members []domain.ExecutionGroupIdleMember) bool {
	stale := event.Type == domain.EventGroupMemberStale
	if stale {
		members = evaluation.staleMembers
	}
	return evaluation.Checks[event.Type].Armed && event.GroupIdle != nil && (stale || event.GroupIdle.GroupActivityAt.Equal(evaluation.GroupActivityAt)) && groupWakeTarget(group) == event.WakeTarget && group.Owner == event.Owner && slices.EqualFunc(event.GroupIdle.Members, members, func(before, after domain.ExecutionGroupIdleMember) bool {
		return before.IntegrationID == after.IntegrationID && before.FeatureID == after.FeatureID && (!stale || before.ProgressClockAt != nil && before.ProgressClockAt.Equal(*after.ProgressClockAt))
	})
}

func groupIdleEventCurrent(st *domain.State, event domain.ExecutionEvent, now time.Time) bool {
	for _, group := range st.ExecutionGroups {
		if group.ID == event.WakeTarget.GroupID && group.ProductID == event.ProductID {
			evaluation, members := evaluateGroupIdle(st, group, now)
			if !groupIdleSameEpisode(event, group, evaluation, members) {
				return false
			}
			history := groupIdleHistory{}
			if event.Type == domain.EventGroupOwnerBlocked {
				history = indexGroupIdleHistory(st)[group.ID]
			}
			return !now.Before(history.dueAt(evaluation, event.Type))
		}
	}
	return false
}

type groupIdleHistory struct {
	latest         map[string]int
	lastReminderAt time.Time
}

func indexGroupIdleHistory(st *domain.State) map[string]groupIdleHistory {
	history := map[string]groupIdleHistory{}
	for index, event := range st.ExecutionEvents {
		if !domain.IsGroupIdleEvent(event.Type) {
			continue
		}
		groupID := event.WakeTarget.GroupID
		entry := history[groupID]
		if entry.latest == nil {
			entry.latest = make(map[string]int, len(groupIdleTypes))
		}
		if previous, exists := entry.latest[event.Type]; !exists || st.ExecutionEvents[previous].ConditionGeneration < event.ConditionGeneration {
			entry.latest[event.Type] = index
		}
		if event.Type == domain.EventGroupOwnerBlocked {
			for _, ack := range event.Acknowledgements {
				if ack.Outcome == "delivered" {
					entry.lastReminderAt = laterActivity(entry.lastReminderAt, &ack.At)
				}
			}
		}
		history[groupID] = entry
	}
	return history
}

func (history groupIdleHistory) dueAt(evaluation groupIdleEvaluation, eventType string) time.Time {
	due := *evaluation.Checks[eventType].NextDueAt
	if eventType == domain.EventGroupOwnerBlocked && !history.lastReminderAt.IsZero() {
		nextReminder := history.lastReminderAt.Add(time.Duration(evaluation.Policy.OwnerBlockedIntervalSeconds) * time.Second)
		due = laterActivity(due, &nextReminder)
	}
	return due
}

func groupIdleStatus(st *domain.State, group domain.ExecutionGroup, now time.Time) groupIdleEvaluation {
	result, members := evaluateGroupIdle(st, group, now)
	history := indexGroupIdleHistory(st)[group.ID]
	for eventType, check := range result.Checks {
		if !check.Armed {
			continue
		}
		due := history.dueAt(result, eventType)
		if index, exists := history.latest[eventType]; exists {
			event := st.ExecutionEvents[index]
			if event.Resolution.ResolvedAt == nil && groupIdleSameEpisode(event, group, result, members) {
				due = laterActivity(due, &event.Delivery.NextAttemptAt)
			}
		}
		check.NextDueAt = &due
		result.Checks[eventType] = check
		if result.NextDueAt == nil || due.Before(*result.NextDueAt) {
			result.NextDueAt = &due
		}
	}
	return result
}

func reconcileGroupIdle(st *domain.State, now time.Time) bool {
	changed := false
	histories := indexGroupIdleHistory(st)
	for _, group := range st.ExecutionGroups {
		history := histories[group.ID]
		if history.latest == nil && (group.ClosedAt != nil || group.OrchestratorHarness.Kind == "") {
			continue
		}
		evaluation, members := evaluateGroupIdle(st, group, now)
		for _, eventType := range groupIdleTypes {
			check := evaluation.Checks[eventType]
			generation := uint64(1)
			if index, exists := history.latest[eventType]; exists {
				previous := &st.ExecutionEvents[index]
				generation = previous.ConditionGeneration + 1
				if previous.Resolution.ResolvedAt == nil {
					if groupIdleSameEpisode(*previous, group, evaluation, members) && !now.Before(*check.NextDueAt) {
						continue
					}
					retireExecutionEvent(st, previous, now, "group idle condition cleared")
					changed = true
				}
			}
			if !check.Armed || now.Before(history.dueAt(evaluation, eventType)) {
				continue
			}
			wake := groupWakeTarget(group)
			payloadMembers := members
			if eventType == domain.EventGroupMemberStale {
				payloadMembers = evaluation.staleMembers
			}
			event := domain.ExecutionEvent{ID: id(), ProductID: group.ProductID, Owner: group.Owner, GroupIDs: []string{group.ID}, Type: eventType, ConditionGeneration: generation, DedupeKey: fmt.Sprintf("%s|%s|%d", group.ID, eventType, generation), WakeTarget: wake, SessionRef: wake.SessionRef, HarnessKind: wake.Kind, TmuxTarget: wake.TmuxTarget, CreatedAt: now, Delivery: domain.ExecutionEventDelivery{NextAttemptAt: now}, GroupIdle: &domain.ExecutionGroupIdlePayload{GroupID: group.ID, GroupActivityAt: evaluation.GroupActivityAt, Members: payloadMembers}}
			st.ExecutionEvents = append(st.ExecutionEvents, event)
			appendExecutionEventAudit(st, event, "execution_event_created", "system/execution-reconciler", now, map[string]any{"type": eventType, "generation": generation})
			changed = true
		}
	}
	return changed
}
