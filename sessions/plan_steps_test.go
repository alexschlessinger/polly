package sessions

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestParsePlanStepsOnlyCapturesOutcomes(t *testing.T) {
	text := "# Plan\r\n- outside\r\n\n~~~md\n## Steps\n- fake\n~~~\n## Steps ##\r\n" +
		"- [x] Keep original **wording**   \n  1. nested outcome\n    - nested again\n  wrapped prose\n" +
		"Notes: - not an item\n  ````go\n```\n- fake\n## Heading\n````\n" +
		"- - -\n***\n1. [ ] Verify café ordering\n7) ship it\n+ [docs](x)\n* [X] Test\n\n## Notes\n- outside again\n"
	want := []string{"Keep original **wording**", "Verify café ordering", "ship it", "[docs](x)", "Test"}
	got, err := ParsePlanSteps(text)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("steps = %#v, %v; want %#v", got, err, want)
	}
	if PlanRevision(text) == PlanRevision(strings.ReplaceAll(text, "\r\n", "\n")) {
		t.Fatal("revision ignored exact line endings")
	}
}

func TestParsePlanStepsRejectsInvalidReviews(t *testing.T) {
	for name, text := range map[string]string{
		"empty": "", "no heading": "# Plan\n- one", "no list": "## Steps\nDo it", "nested only": "## Steps\n  - not a top-level step",
		"empty item": "## Steps\n- one\n- \t", "empty numbered item": "## Steps\n1.", "checked only": "## Steps\n- [X]",
		"duplicate title": "## Steps\n1. One\n* [x] one", "duplicate section": "## Steps\n- one\n## Steps\n- two", "unclosed fence": "## Steps\n- one\n```yaml\n",
		"controls": "## Steps\n- \x1b[4m", "invalid UTF8": "## Steps\n- \xff", "non-line-ending CR": "## Steps\n- disguise\rname",
		"too large": strings.Repeat("a", MaxPlanBytes+1), "rule only": "## Steps\n- - -",
	} {
		t.Run(name, func(t *testing.T) {
			if steps, err := ParsePlanSteps(text); err == nil {
				t.Fatalf("accepted malformed plan: %#v", steps)
			}
		})
	}
	var text strings.Builder
	text.WriteString("## Steps\n")
	for i := 0; i <= maxPlanSteps; i++ {
		fmt.Fprintf(&text, "- step %d\n", i)
	}
	if _, err := ParsePlanSteps(text.String()); err == nil {
		t.Fatal("accepted too many steps")
	}
}
