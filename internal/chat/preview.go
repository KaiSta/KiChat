package chat

import (
	"context"
	"fmt"

	"kichat/internal/llm"
)

// PreviewBudget reports the context budget a chat would have with the given
// model, without changing anything. The model switcher uses it to warn that a
// smaller context window will not fit the current conversation before the user
// commits to the switch.
func (e *Engine) PreviewBudget(ctx context.Context, chatID, backendID, modelID string) (ContextBudget, error) {
	c, err := e.Store.GetChat(chatID)
	if err != nil {
		return ContextBudget{}, err
	}
	settings := e.Settings()

	if backendID == "" {
		backendID = c.Backend
	}
	if modelID == "" {
		modelID = c.Model
	}

	var model llm.ModelInfo
	var provider llm.Provider
	if backendID != "" && modelID != "" {
		model = e.resolveModel(ctx, backendID, modelID, settings)
		provider, _ = e.Registry.Provider(backendID)
	} else {
		return ContextBudget{}, fmt.Errorf("kein Modell ausgewählt")
	}

	assembly, _, err := e.buildAssembly(ctx, &c, provider, backendID, modelID, settings, model, "")
	if err != nil {
		return ContextBudget{}, err
	}
	return assembly.Budget, nil
}
