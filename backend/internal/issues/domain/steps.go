package domain

type StepKind string

const (
	Reopen          StepKind = "reopen"
	AddLabels       StepKind = "add_labels"
	RemoveLabel     StepKind = "remove_label"
	RemoveAssignees StepKind = "remove_assignees"
	AddAssignees    StepKind = "add_assignees"
	Close           StepKind = "close"
)

type Step struct {
	Kind  StepKind
	Names []string
}

func PlanUpdate(d UpdateDraft) []Step {
	var steps []Step
	if d.State == "open" {
		steps = append(steps, Step{Kind: Reopen})
	}
	if len(d.Labels.Add) > 0 {
		steps = append(steps, Step{Kind: AddLabels, Names: d.Labels.Add})
	}
	for _, name := range d.Labels.Remove {
		steps = append(steps, Step{Kind: RemoveLabel, Names: []string{name}})
	}
	if len(d.Assignees.Remove) > 0 {
		steps = append(steps, Step{Kind: RemoveAssignees, Names: d.Assignees.Remove})
	}
	if len(d.Assignees.Add) > 0 {
		steps = append(steps, Step{Kind: AddAssignees, Names: d.Assignees.Add})
	}
	if d.State == "closed" {
		steps = append(steps, Step{Kind: Close})
	}
	return steps
}
