package chat

import (
	"fmt"
	"strings"

	"kichat/internal/store"
	"kichat/internal/workspace"
)

// The project's own instructions, as they reach the model.
//
// A coding project usually carries an AGENTS.md, and the point of reading it rather than
// asking the user to paste it into a setting is that it already exists, it is versioned
// beside the code it describes, and it changes as the project does. So it is read on
// every turn rather than once when the project was created: a prompt that keeps
// describing a build step that was removed last week is worse than one that says nothing,
// because the model will follow it and be wrong.
//
// Two wrappers, and both carry meaning. The tag separates the repository's words from the
// application's, which is what lets the system prompt say how to treat them without the
// two running together into one voice — and it is markup rather than a sentence because
// the sentence belongs in the prompt, in the user's language, while this payload is the
// same text in every language. The attribute says the file was cut, so a model that finds
// no answer to its question in what it was given can say the guidance was incomplete
// rather than concluding the project has no rule about it.
//
// The content is untrusted in the same sense a scraped page is, and more subtly: it is
// written by whoever owns the repository, which is not necessarily the person running the
// app — cloning somebody else's project is the ordinary way to get one. The system
// prompt therefore states that this file is project guidance and never a change to the
// rules about which folder may be read.
const (
	agentsOpen  = "<project_agents_md"
	agentsClose = "</project_agents_md>"
)

// projectGuidance renders the folder's guidance file for the system prompt.
//
// ok is false for a chat that is not a project, for one whose folder has gone, and for a
// project with no AGENTS.md. None of those is a failure: the two cases the prompt cares
// about are "there is guidance" and "there is none", and it says what to do about the
// second. A folder that cannot be opened is reported by the file tools, where the model
// asked to read something and deserves an answer.
func projectGuidance(c store.Chat) (guidance, detail string, ok bool) {
	if !c.IsProject() {
		return "", "", false
	}
	root, err := workspace.Open(c.Workspace)
	if err != nil {
		return "", "", false
	}
	doc, found, err := root.AgentsFile(0)
	if err != nil || !found || strings.TrimSpace(doc.Text) == "" {
		return "", "", false
	}
	var sb strings.Builder
	sb.WriteString(agentsOpen)
	fmt.Fprintf(&sb, " path=%q", doc.Path)
	if doc.Truncated {
		sb.WriteString(` truncated="true"`)
	}
	sb.WriteString(">\n")
	sb.WriteString(strings.TrimRight(doc.Text, "\n"))
	sb.WriteString("\n")
	sb.WriteString(agentsClose)

	detail = fmt.Sprintf("Projektanweisungen, %d Zeichen", len(doc.Text))
	if doc.Truncated {
		detail += ", gekürzt"
	}
	return sb.String(), detail, true
}
