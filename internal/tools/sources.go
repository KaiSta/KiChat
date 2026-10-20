package tools

import (
	"context"
	"strings"
	"sync"

	"kichat/internal/store"
)

// SourceList numbers the web sources one turn consults.
//
// The numbering is the whole point. Every result list used to start at [1], so an answer
// that followed three searches had three different sites called [1] — the model could not
// name one of them unambiguously, and neither the reader nor the app could resolve a
// citation. One list per turn, shared by the pre-flight search and every tool call in it,
// means the numbers continue where the previous list stopped, so `[17]` is one URL for
// the rest of that turn.
//
// A URL already in the list keeps its first number: the same page found by two queries is
// one source, and splitting it into two citations would make the list lie about how many
// sites an answer used.
//
// It is guarded because one turn's tool calls are separate steps but the app runs several
// chats at once, and a tool may be called from a goroutine a provider spawned.
type SourceList struct {
	mu      sync.Mutex
	sources []store.Source
	byURL   map[string]int
}

// NewSourceList starts the numbering of one turn.
func NewSourceList() *SourceList {
	return &SourceList{byURL: map[string]int{}}
}

// Add numbers a source and returns the number the model is told to cite. The URL is the
// identity; a result with no URL (which a provider occasionally returns) still gets a
// number, because the next result's number must not depend on it being linkable.
func (l *SourceList) Add(title, url string) int {
	if l == nil {
		return 0
	}
	title = strings.TrimSpace(title)
	url = strings.TrimSpace(url)
	l.mu.Lock()
	defer l.mu.Unlock()
	if url != "" {
		if index, ok := l.byURL[url]; ok {
			return index
		}
	}
	l.sources = append(l.sources, store.Source{Index: len(l.sources) + 1, Title: title, URL: url})
	index := len(l.sources)
	if url != "" {
		l.byURL[url] = index
	}
	return index
}

// All returns the sources in the order they were numbered, as a copy: the answer is
// written from it after the turn's last tool call, and a slice the registry keeps
// mutating would be a list that changes under the caller.
func (l *SourceList) All() []store.Source {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]store.Source(nil), l.sources...)
}

// SourcesFrom returns the source list of the turn a tool call belongs to, nil when the
// call is not part of one — a service exercised directly by a test, or a tool run from
// somewhere that has no turn.
func SourcesFrom(ctx context.Context) *SourceList {
	turn, _ := ctx.Value(turnKey{}).(TurnRef)
	return turn.Sources
}
