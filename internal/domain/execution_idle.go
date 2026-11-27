package domain

import "time"

const (
	EventGroupIdle             = "group_idle"
	EventGroupWaitTimeout      = "group_wait_timeout"
	EventGroupMemberActionable = "group_member_actionable"
	EventGroupOwnerBlocked     = "group_owner_blocked"
	MaxExecutionIdleSeconds    = 7 * 24 * 60 * 60
)

func IsGroupIdleEvent(eventType string) bool {
	switch eventType {
	case EventGroupIdle, EventGroupWaitTimeout, EventGroupMemberActionable, EventGroupOwnerBlocked:
		return true
	}
	return false
}

type ExecutionIdlePolicy struct {
	IdleAfterSeconds            int `json:"idle_after_seconds"`
	WaitTimeoutSeconds          int `json:"wait_timeout_seconds"`
	OwnerBlockedIntervalSeconds int `json:"owner_blocked_interval_seconds"`
}

type ExecutionGroupIdlePayload struct {
	GroupID         string                     `json:"group_id"`
	GroupActivityAt time.Time                  `json:"group_activity_at"`
	Members         []ExecutionGroupIdleMember `json:"members"`
}

type ExecutionGroupIdleMember struct {
	IntegrationID            string `json:"integration_id"`
	FeatureID                string `json:"feature_id"`
	State                    string `json:"state"`
	DesiredState             string `json:"desired_state"`
	SecondsSinceLastProgress *int64 `json:"seconds_since_last_progress"`
	SecondsSinceHeartbeat    *int64 `json:"seconds_since_heartbeat"`
	NextAction               string `json:"next_action"`
}
