package chat

import (
	"encoding/json"
	"testing"

	"kichat/internal/config"
)

// TestUnresolvedVarsMarshalsToAnEmptyList is the regression test for the render
// error reported on the system-prompt tab. The prompt editor calls .length on
// this result, and a nil slice marshals to JSON null rather than [], so "every
// variable is known" arrived in the UI as null and threw during render.
func TestUnresolvedVarsMarshalsToAnEmptyList(t *testing.T) {
	// The shipped default prompt uses only known variables, which is exactly the
	// state the app starts in.
	got := UnresolvedVars(config.DefaultSystemPrompt)
	if got == nil {
		t.Fatal("UnresolvedVars returned nil for a prompt with no unknown variables")
	}
	if len(got) != 0 {
		t.Errorf("UnresolvedVars = %v, want an empty list", got)
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "[]" {
		t.Errorf("marshalled to %s, want [] — anything else reaches the UI as null", raw)
	}
}

// TestUnresolvedVarsStillReportsTypos guards the other direction: making the
// empty case safe must not silence a real typo, and the list stays sorted so the
// warning does not reshuffle between keystrokes.
func TestUnresolvedVarsStillReportsTypos(t *testing.T) {
	// werkzeug_liste is a plausible misspelling of tool_list and is reported.
	// tool-list does not match the variable pattern at all (no hyphen), so it is
	// ordinary text rather than a variable and is deliberately not reported.
	got := UnresolvedVars("{{datetime}} {{werkzeug_liste}} {{tool-list}} {{bogus}}")
	want := []string{"bogus", "werkzeug_liste"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("UnresolvedVars = %v, want %v (sorted, and without the non-matching form)", got, want)
	}
}
