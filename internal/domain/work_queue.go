package domain

import "time"

type QuestionDetails struct {
	AddressedTo      string           `json:"addressed_to"`
	AskedBy          string           `json:"asked_by"`
	AskedByUser      string           `json:"asked_by_user,omitempty"`
	Kind             string           `json:"kind"`
	Options          []QuestionOption `json:"options,omitempty"`
	Recommendation   string           `json:"recommendation,omitempty"`
	ContextRefs      []string         `json:"context_refs,omitempty"`
	Blocking         bool             `json:"blocking"`
	Answer           *QuestionAnswer  `json:"answer,omitempty"`
	DecisionFor      string           `json:"decision_for,omitempty"`
	DecisionAction   string           `json:"decision_action,omitempty"`
	DecisionRevision int64            `json:"decision_revision,omitempty"`
	WithdrawnReason  string           `json:"withdrawn_reason,omitempty"`
}

type QuestionOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Consequence string `json:"consequence"`
}

type AnswerValidation struct {
	Verdict string `json:"verdict"`
	Note    string `json:"note"`
}

type QuestionAnswer struct {
	Text       string `json:"text"`
	OptionID   string `json:"option_id,omitempty"`
	AnsweredBy string `json:"answered_by"`
	// AnsweredByUser is the human the answer speaks for (the agent owner for a relayed chat answer).
	AnsweredByUser string           `json:"answered_by_user,omitempty"`
	AnsweredAt     time.Time        `json:"answered_at"`
	Validation     AnswerValidation `json:"validation"`
	Override       bool             `json:"override"`
}

type FeatureDependency struct {
	Meta
	FromFeatureID      string     `json:"from_feature_id"`
	ToFeatureID        string     `json:"to_feature_id"`
	Type               string     `json:"type"`
	Status             string     `json:"status"`
	Rationale          string     `json:"rationale"`
	ProposedBy         string     `json:"proposed_by"`
	ApprovalQuestionID string     `json:"approval_question_id,omitempty"`
	DecidedBy          string     `json:"decided_by,omitempty"`
	DecidedAt          *time.Time `json:"decided_at,omitempty"`
}

type UserOrchestrator struct {
	Meta
	UserID         string           `json:"user_id"`
	Products       []string         `json:"products"`
	Harness        ExecutionHarness `json:"harness"`
	Available      bool             `json:"available"`
	Generation     uint64           `json:"generation"`
	UnregisteredAt *time.Time       `json:"unregistered_at,omitempty"`
}

var WorkActions = []string{
	"ask_question", "answer_questions", "withdraw_question", "set_idea_promotion", "request_idea_promotion",
	"propose_dependency", "decide_dependency", "request_dependency_decision", "register_user_orchestrator", "unregister_user_orchestrator",
}
