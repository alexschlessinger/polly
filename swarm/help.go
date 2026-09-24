package swarm

import (
	"context"
	_ "embed"
	"fmt"
	"regexp"
	"strings"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

// Bundle the reference with its runtime version; help never reads checkout files.
//
//go:embed help.md
var coordinationGuide string

//go:embed workflow_help.md
var workflowGuide string

// workflowExample is one "## ... example" section of the guide, addressed by
// the workflow name its code defines.
type workflowExample struct{ name, title, body string }

var workflowReference, workflowExamples = splitWorkflowGuide(workflowGuide)

var workflowNamePattern = regexp.MustCompile(`polly\.workflow\("([^"]+)"`)

// splitWorkflowGuide separates the reference from the runnable examples so a
// caller loads the one example it needs instead of all of them. An index
// takes the examples' place in the reference.
func splitWorkflowGuide(guide string) (string, []workflowExample) {
	var parts []string
	var examples []workflowExample
	index := -1
	sections := strings.Split(guide, "\n## ")
	parts = append(parts, sections[0])
	for _, section := range sections[1:] {
		title, _, _ := strings.Cut(section, "\n")
		if !strings.HasSuffix(title, " example") {
			parts = append(parts, "## "+section)
			continue
		}
		match := workflowNamePattern.FindStringSubmatch(section)
		if match == nil {
			panic("workflow help example without a polly.workflow name: " + title)
		}
		if index < 0 {
			index = len(parts)
			parts = append(parts, "")
		}
		examples = append(examples, workflowExample{name: match[1], title: strings.TrimSuffix(title, " example"), body: "## " + strings.TrimRight(section, "\n") + "\n"})
	}
	if index < 0 {
		panic("workflow help has no examples")
	}
	var list strings.Builder
	list.WriteString("## Examples\n\nEach example is a complete, tested script. Load one at a time with `workflow_help({example: NAME})`:\n\n")
	for _, example := range examples {
		fmt.Fprintf(&list, "- `%s`: %s\n", example.name, example.title)
	}
	parts[index] = list.String()
	return strings.Join(parts, "\n"), examples
}

// Members cannot load the parent's playbook. Keep their privacy, waiting, and
// budget rules in the role prompt, including for library hosts without CLI defaults.
const delegationGuidance = `Use list_agents for teammate names and execution/task state. send_message({target,message}) provides information without starting idle workers. Parents use followup_task({target,message}) to continue workers and interrupt_agent({target}) to interrupt a turn. A worker stopped by the user cannot be resumed by a parent follow-up; wait for the user to resume it. Use wait_agent({}) to park for updates; timeout_ms is optional. Results arrive automatically and swarm_read shows saved evidence or decisions. An idle worker may have unfinished work. Use these tool names when older conversation entries differ.`

const memberCoordinationGuidance = `Private conversations remain private. Use wait_agent when waiting on teammates instead of sleeping or re-reading reports. You inherit the host's model-call limit; do not impose a smaller iteration cap. Budget exhaustion alone does not mean work stalled: retain your assignment and findings, and report the explicit allowance needed to continue. Additional iteration grants require a user-directed client action.`

func registerHelpTools(registry *tools.ToolRegistry) {
	registry.Register(&tools.Func{
		Name: "swarm_help",
		Desc: "Read the coordination guide before coordinating agents or workflows. Reuse it while available; reload when needed.",
		Run: func(context.Context, tools.Args) (string, error) {
			return coordinationGuide, nil
		},
	})
	registry.MarkAlwaysAllowed("swarm_help")
	names := make([]string, 0, len(workflowExamples))
	for _, example := range workflowExamples {
		names = append(names, example.name)
	}
	available := strings.Join(names, ", ")
	registry.Register(&tools.Func{
		Name: "workflow_help",
		Desc: "Read the JavaScript workflow API reference before writing or changing a workflow script; it indexes runnable examples to load one at a time by name. Reuse while available; reload when needed.",
		Params: schema.Params{
			"example": schema.S("Return this one example instead of the reference: " + available),
		},
		Run: func(_ context.Context, a tools.Args) (string, error) {
			name := a.String("example")
			if name == "" {
				return workflowReference, nil
			}
			for _, example := range workflowExamples {
				if example.name == name {
					return example.body, nil
				}
			}
			return "", fmt.Errorf("unknown workflow example %q; available: %s", name, available)
		},
	})
	registry.MarkAlwaysAllowed("workflow_help")
}
