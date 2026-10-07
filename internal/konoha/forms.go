package konoha

import (
	"encoding/json"
	"fmt"
	"strings"
)

// #120 typed interaction forms for human input.
//
// Forms are schema-driven, typed wrappers around the EXISTING §18/§55 decision
// actions (#108/#97). A form submission is validated fail-closed (§36), mapped
// onto one existing decision action, redacted (#82), then written through the
// SAME decision channel (SubmitHumanDecision / WriteConsoleDecision) so audit
// (§55) and idempotency (§24) are inherited — never a parallel source of truth
// (§44 #20), never a new lifecycle state.
//
// Form ids and their mapped §18 actions:
//   approve          -> approve
//   request-changes  -> request-changes
//   clarification    -> resume
//
// A clarification form is answered at the existing §55 approval gate: the gate
// pauses the cycle (PENDING_APPROVAL) and a valid answer maps to `resume`, which
// `EvaluateApproval`/`ApplyHumanDecision` treat as continue; a timeout keeps the
// existing BLOCKED semantics (§40).

// formFieldType is the closed set of supported field types.
type formFieldType string

const (
	formFieldText   formFieldType = "text"
	formFieldChoice formFieldType = "choice"
	formFieldNumber formFieldType = "number"
)

// FormField declares one typed input field. A text field carries MaxLength (a
// byte cap); a choice field carries its closed Options set; a number field may
// carry Min/Max bounds.
type FormField struct {
	Name      string        `json:"name"`
	Label     string        `json:"label,omitempty"`
	Type      formFieldType `json:"type"`
	Required  bool          `json:"required,omitempty"`
	Options   []string      `json:"options,omitempty"`
	MaxLength int           `json:"maxLength,omitempty"`
	Min       *float64      `json:"min,omitempty"`
	Max       *float64      `json:"max,omitempty"`
}

// FormSchema is the deterministic schema for one decision form. Decision is the
// form id; Action is the existing §18 action it maps onto. FeedbackField names
// the field whose value becomes the decision feedback (empty for approve).
type FormSchema struct {
	Decision      string      `json:"decision"`
	Action        string      `json:"action"`
	Title         string      `json:"title,omitempty"`
	Description   string      `json:"description,omitempty"`
	FeedbackField string      `json:"feedbackField,omitempty"`
	Fields        []FormField `json:"fields,omitempty"`
}

// FormSubmission is one raw form submission from the console or another client.
// Values holds the typed field values as decoded JSON; Comments carries the
// diff-anchored findings (request-changes only).
type FormSubmission struct {
	Decision string         `json:"decision"`
	Values   map[string]any `json:"values,omitempty"`
	Comments []DiffComment  `json:"comments,omitempty"`
	// Attempt and SHA bind the submission to the exact gate the human saw; a
	// superseded attempt/SHA is stale and refused (§108/#120).
	Attempt int    `json:"attempt,omitempty"`
	SHA     string `json:"sha,omitempty"`
	Actor   string `json:"actor,omitempty"`
}

// FormAnswer is one validated answer in schema order, retained for evidence.
type FormAnswer struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Text     string  `json:"text,omitempty"`
	Number   float64 `json:"number,omitempty"`
	IsNumber bool    `json:"isNumber,omitempty"`
}

// FormDecision is a validated form mapped onto an existing decision: the §18
// action, the synthesized feedback body, the typed answers, and any anchored
// comments. It is pure data — not yet a decision.
type FormDecision struct {
	Action   string
	Feedback string
	Answers  []FormAnswer
	Comments []DiffComment
}

// decisionFormSchemas returns the closed catalog of decision forms in a
// deterministic order.
func decisionFormSchemas() []FormSchema {
	return []FormSchema{
		{
			Decision:    "approve",
			Action:      humanDecisionApprove,
			Title:       "Approve",
			Description: "Grant the gate and continue the pipeline.",
		},
		{
			Decision:      "request-changes",
			Action:        humanDecisionRequestChanges,
			Title:         "Request changes",
			Description:   "Send the work back for a bounded rework.",
			FeedbackField: "summary",
			Fields: []FormField{
				{Name: "summary", Label: "Summary", Type: formFieldText, Required: true, MaxLength: maxDecisionFeedback},
				{Name: "severity", Label: "Severity", Type: formFieldChoice, Options: []string{"low", "medium", "high"}},
			},
		},
		{
			Decision:      "clarification",
			Action:        humanDecisionResume,
			Title:         "Clarification",
			Description:   "Answer the outstanding question and resume the cycle.",
			FeedbackField: "answer",
			Fields: []FormField{
				{Name: "answer", Label: "Answer", Type: formFieldText, Required: true, MaxLength: maxDecisionFeedback},
				{Name: "confidence", Label: "Confidence", Type: formFieldNumber, Min: floatPtr(0), Max: floatPtr(1)},
			},
		},
	}
}

// lookupFormSchema finds a schema by form id. ok=false for an unknown id, so an
// unrecognized form can never become a decision (§36).
func lookupFormSchema(decision string) (FormSchema, bool) {
	for _, s := range decisionFormSchemas() {
		if s.Decision == decision {
			return s, true
		}
	}
	return FormSchema{}, false
}

// ValidateFormSubmission deterministically gates one form submission against its
// schema: unknown decision/field, missing required value, wrong type, an option
// outside the closed set, and an over-cap or out-of-range value all fail closed
// (§36). The result is a FormDecision mapped onto an existing §18 action. Pure;
// no browser or Temporal required.
func ValidateFormSubmission(sub FormSubmission) (FormDecision, error) {
	schema, ok := lookupFormSchema(strings.TrimSpace(sub.Decision))
	if !ok {
		return FormDecision{}, fmt.Errorf("form %q not in the closed set {approve, request-changes, clarification}", sub.Decision)
	}
	byName := make(map[string]FormField, len(schema.Fields))
	for _, f := range schema.Fields {
		byName[f.Name] = f
	}
	for name := range sub.Values {
		if _, ok := byName[name]; !ok {
			return FormDecision{}, fmt.Errorf("form %q: unknown field %q", schema.Decision, name)
		}
	}
	fd := FormDecision{Action: schema.Action}
	for _, f := range schema.Fields {
		raw, present := sub.Values[f.Name]
		if !present || isEmptyValue(raw) {
			if f.Required {
				return FormDecision{}, fmt.Errorf("form %q: field %q is required", schema.Decision, f.Name)
			}
			continue
		}
		ans, err := validateFormField(schema.Decision, f, raw)
		if err != nil {
			return FormDecision{}, err
		}
		fd.Answers = append(fd.Answers, ans)
		if f.Name == schema.FeedbackField {
			fd.Feedback = strings.TrimSpace(ans.Text)
		}
	}
	if len(sub.Comments) > 0 {
		if schema.Action != humanDecisionRequestChanges {
			return FormDecision{}, fmt.Errorf("form %q carries anchored comments but does not map to request-changes", schema.Decision)
		}
		fd.Comments = append([]DiffComment(nil), sub.Comments...)
	}
	return fd, nil
}

// validateFormField checks one present value against its field declaration and
// returns the typed answer.
func validateFormField(decision string, f FormField, raw any) (FormAnswer, error) {
	switch f.Type {
	case formFieldText:
		s, ok := raw.(string)
		if !ok {
			return FormAnswer{}, fmt.Errorf("form %q: field %q must be a string", decision, f.Name)
		}
		s = strings.TrimSpace(s)
		if s == "" {
			if f.Required {
				return FormAnswer{}, fmt.Errorf("form %q: field %q is required", decision, f.Name)
			}
			return FormAnswer{}, nil
		}
		cap := f.MaxLength
		if cap <= 0 {
			cap = maxDecisionFeedback
		}
		if len(s) > cap {
			return FormAnswer{}, fmt.Errorf("form %q: field %q (%d bytes) exceeds the %d-byte cap", decision, f.Name, len(s), cap)
		}
		return FormAnswer{Name: f.Name, Type: string(f.Type), Text: s}, nil
	case formFieldChoice:
		s, ok := raw.(string)
		if !ok {
			return FormAnswer{}, fmt.Errorf("form %q: field %q must be a string choice", decision, f.Name)
		}
		for _, o := range f.Options {
			if s == o {
				return FormAnswer{Name: f.Name, Type: string(f.Type), Text: s}, nil
			}
		}
		return FormAnswer{}, fmt.Errorf("form %q: field %q value %q not in %v", decision, f.Name, s, f.Options)
	case formFieldNumber:
		n, ok := toFloat64(raw)
		if !ok {
			return FormAnswer{}, fmt.Errorf("form %q: field %q must be a number", decision, f.Name)
		}
		if f.Min != nil && n < *f.Min {
			return FormAnswer{}, fmt.Errorf("form %q: field %q (%v) is below the minimum %v", decision, f.Name, n, *f.Min)
		}
		if f.Max != nil && n > *f.Max {
			return FormAnswer{}, fmt.Errorf("form %q: field %q (%v) is above the maximum %v", decision, f.Name, n, *f.Max)
		}
		return FormAnswer{Name: f.Name, Type: string(f.Type), Number: n, IsNumber: true}, nil
	}
	return FormAnswer{}, fmt.Errorf("form %q: field %q has unknown type %q", decision, f.Name, f.Type)
}

// isEmptyValue reports whether a decoded JSON value is absent-equivalent: nil or
// an all-whitespace string.
func isEmptyValue(v any) bool {
	if v == nil {
		return true
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s) == ""
	}
	return false
}

// toFloat64 coerces a decoded JSON number to float64. Anything non-numeric fails
// closed.
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// decisionRequest binds a validated form onto the live gate identity (attempt +
// SHA), applies the #82 redactor to the freeform feedback and comment bodies,
// and yields the DecisionRequest the existing §18 writer consumes. The gate
// fields (owner/repo/issue/cycleId/attempt/sha/actor) come from the caller.
func (fd FormDecision) decisionRequest(base DecisionRequest) DecisionRequest {
	out := base
	out.Action = fd.Action
	out.Feedback = defaultRedactor.Redact(fd.Feedback)
	out.Comments = make([]DiffComment, 0, len(fd.Comments))
	for _, c := range fd.Comments {
		c.Body = defaultRedactor.Redact(c.Body)
		out.Comments = append(out.Comments, c)
	}
	return out
}
