package swarm

import (
	"context"
	_ "embed"

	"github.com/alexschlessinger/pollytool/tools"
)

// Bundle the reference with its runtime version; help never reads checkout files.
//
//go:embed help.md
var coordinationGuide string

//go:embed workflow_help.md
var workflowGuide string

// Members cannot load the parent's playbook. Keep their privacy, waiting, and
// budget rules in the role prompt, including for library hosts without CLI defaults.
const memberCoordinationGuidance = `Private conversations remain private; share findings explicitly. Use swarm_wait when waiting on teammates instead of sleeping or re-reading reports. You inherit the host's model-call limit; do not impose a smaller iteration cap. Budget exhaustion alone does not mean work stalled: retain your assignment and findings, and report the explicit allowance needed to continue. Additional iteration grants require a user-directed client action.`

func registerHelpTools(registry *tools.ToolRegistry) {
	registry.Register(&tools.Func{
		Name: "swarm_help",
		Desc: "Read the coordination guide before coordinating agents or workflows. Reuse it while available; reload when needed.",
		Run: func(context.Context, tools.Args) (string, error) {
			return coordinationGuide, nil
		},
	})
	registry.MarkAlwaysAllowed("swarm_help")
	registry.Register(&tools.Func{
		Name: "workflow_help",
		Desc: "Read the JavaScript workflow API and examples before writing or changing a workflow script. Reuse while available; reload when needed.",
		Run: func(context.Context, tools.Args) (string, error) {
			return workflowGuide, nil
		},
	})
	registry.MarkAlwaysAllowed("workflow_help")
}
