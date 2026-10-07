// Package chat assembles context, drives the model/tool loop, and manages
// compaction.
package chat

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// TemplateVars are the values available to a system prompt.
type TemplateVars struct {
	Now          time.Time
	Location     string
	Model        string
	Backend      string
	ContextLimit int
	ContextUsed  int
	Tools        []string
}

var varPattern = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)

// KnownVars lists the variables the template engine understands.
var KnownVars = []string{
	"datetime", "date", "time", "weekday", "location",
	"model", "backend", "context_limit", "context_used", "tool_list",
}

var germanWeekdays = map[time.Weekday]string{
	time.Monday:    "Montag",
	time.Tuesday:   "Dienstag",
	time.Wednesday: "Mittwoch",
	time.Thursday:  "Donnerstag",
	time.Friday:    "Freitag",
	time.Saturday:  "Samstag",
	time.Sunday:    "Sonntag",
}

// RenderPrompt substitutes every known variable. Unknown variables are left
// untouched so a typo is visible in the output rather than silently vanishing.
func RenderPrompt(tmpl string, vars TemplateVars) string {
	if !strings.Contains(tmpl, "{{") {
		return tmpl
	}
	now := vars.Now
	if now.IsZero() {
		now = time.Now()
	}
	zone, _ := now.Zone()

	toolList := "(keine)"
	if len(vars.Tools) > 0 {
		toolList = strings.Join(vars.Tools, ", ")
	}
	contextLimit := "unbekannt"
	if vars.ContextLimit > 0 {
		contextLimit = fmt.Sprintf("%d Tokens", vars.ContextLimit)
	}

	replacements := map[string]string{
		"datetime":      fmt.Sprintf("%s, %s Uhr (%s)", germanWeekdays[now.Weekday()], now.Format("02.01.2006 15:04"), zone),
		"date":          now.Format("02.01.2006"),
		"time":          now.Format("15:04"),
		"weekday":       germanWeekdays[now.Weekday()],
		"location":      vars.Location,
		"model":         vars.Model,
		"backend":       vars.Backend,
		"context_limit": contextLimit,
		"context_used":  fmt.Sprintf("%d Tokens", vars.ContextUsed),
		"tool_list":     toolList,
	}

	return varPattern.ReplaceAllStringFunc(tmpl, func(match string) string {
		name := varPattern.FindStringSubmatch(match)[1]
		if v, ok := replacements[name]; ok {
			return v
		}
		return match
	})
}

// UnresolvedVars returns the template variables that are not known, so the
// prompt editor can warn about typos.
func UnresolvedVars(tmpl string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range varPattern.FindAllStringSubmatch(tmpl, -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		known := false
		for _, k := range KnownVars {
			if k == name {
				known = true
				break
			}
		}
		if !known {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// PreviewPrompt renders a prompt for display in the editor.
func PreviewPrompt(tmpl, location, model string) string {
	return RenderPrompt(tmpl, TemplateVars{
		Now:          time.Now(),
		Location:     location,
		Model:        model,
		Tools:        []string{"search_web", "scrape_url", "search_documents", "read_pages"},
		ContextUsed:  12345,
		ContextLimit: 48128,
	})
}
