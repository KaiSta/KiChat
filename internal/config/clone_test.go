package config

import (
	"fmt"
	"reflect"
	"testing"

	"kichat/internal/llm"
)

// settingsWithEveryContainerFilled is what the walk below needs to test anything: a map or a
// slice left at its zero value has nothing to share, so a clone that failed to copy it would
// pass unnoticed. Each one is filled here rather than in the test so that a schema change
// shows up as a failing walk with the field's own path in the message.
func settingsWithEveryContainerFilled() Settings {
	s := Default()
	s.Backends = []llm.BackendConfig{
		{ID: "one", Name: "One", Kind: "custom", BaseURL: "http://127.0.0.1:1"},
		{ID: "two", Name: "Two", Kind: "custom"},
	}
	s.ModelOverrides = map[string]llm.ModelOverride{
		"one/model": {ContextLimit: 4096, Capabilities: []string{"tools"}},
	}
	s.PromptPresets = []PromptPreset{{ID: "p1", Name: "P", Prompt: "text"}, {ID: "p2"}}
	s.HiddenModels = []string{"one/model"}
	s.ImageProfiles = []ImageProfile{
		{Name: "profile", Settings: ImageSettings{Model: "a-checkpoint"}},
		{Name: "second"},
	}
	s.Search.ProviderOrder = []string{"brave", "duckduckgo"}
	s.Runtime.ModelDirs = []string{"/models", "/more"}
	s.Runtime.Models = map[string]ManagedModel{"one": {Enabled: true, ModelPath: "/models/a.gguf"}}
	return s
}

// TestCloneSharesNothingWithTheOriginal walks the whole value rather than checking the fields
// one by one, because the failure this exists to prevent is a *new* field: a map or a slice
// added to Settings, or to one of the structs it holds, with no line in Clone() is a data race
// the moment a snapshot is marshalled beside a setter. That is not hypothetical — it is how
// the three pairs in the app's own test were found, and nothing else in the suite notices.
//
// Two things are asserted for every container the value holds: that the fixture fills it (so
// comparing it means something) and that the clone's is a different object from the live one.
// A container that is empty fails the first check instead of quietly passing the second.
func TestCloneSharesNothingWithTheOriginal(t *testing.T) {
	original := settingsWithEveryContainerFilled()
	clone := original.Clone()
	if !reflect.DeepEqual(original, clone) {
		t.Fatalf("the clone differs from what it was cloned from:\n%+v\n%+v", original, clone)
	}

	walked := 0
	var walk func(path string, live, copy reflect.Value)
	walk = func(path string, live, copy reflect.Value) {
		switch live.Kind() {
		case reflect.Map, reflect.Slice:
			walked++
			if live.Len() == 0 {
				t.Errorf("%s is empty in the fixture, so a clone that shared it would not be caught: "+
					"fill it here and copy it in Clone()", path)
				return
			}
			if live.UnsafePointer() == copy.UnsafePointer() {
				t.Errorf("%s is the same container in the clone and in the live settings: a snapshot "+
					"that shares it is written by one goroutine while another marshals it", path)
			}
			switch live.Kind() {
			case reflect.Map:
				for _, key := range live.MapKeys() {
					walk(fmt.Sprintf("%s[%q]", path, key.String()), live.MapIndex(key), copy.MapIndex(key))
				}
			case reflect.Slice:
				for i := 0; i < live.Len(); i++ {
					walk(fmt.Sprintf("%s[%d]", path, i), live.Index(i), copy.Index(i))
				}
			}
		case reflect.Pointer, reflect.Interface:
			// Nothing in this schema holds one today, and a field that started to would need
			// a decision about what a snapshot of it is. Saying so beats a silent pass.
			if !live.IsNil() {
				t.Errorf("%s is a %s, which Clone() does not copy: decide what a snapshot of it means",
					path, live.Kind())
			}
		case reflect.Struct:
			for i := 0; i < live.NumField(); i++ {
				field := live.Type().Field(i)
				if !field.IsExported() {
					continue
				}
				walk(path+"."+field.Name, live.Field(i), copy.Field(i))
			}
		}
	}
	walk("Settings", reflect.ValueOf(original), reflect.ValueOf(clone))

	// The number of containers the value holds, counted: a walk that stopped matching — a
	// renamed field, a struct that is no longer reached — would otherwise pass without
	// having looked at anything. Move it together with the fixture when the schema gains one.
	const containers = 9
	if walked != containers {
		t.Errorf("walked %d containers, want %d: the fixture, this number and Clone() have to "+
			"move together when the schema gains a map or a slice", walked, containers)
	}
}
