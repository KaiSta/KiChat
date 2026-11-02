package chat

import (
	"context"

	"kichat/internal/llm"
	"kichat/internal/uimsg"
)

// PreviewBudget reports the context budget a chat would have with the given
// model, without changing anything. The model switcher uses it to warn that a
// smaller context window will not fit the current conversation before the user
// commits to the switch.
//
// The model is resolved from the catalogue already in hand rather than by asking
// the backends again: this runs inside GetChatState, which the UI awaits when a
// chat is opened, and a listing request there could stall the window on a backend
// that is merely slow (see resolveModelCached). ctx is kept in the signature so a
// future caller can bound whatever replaces this.
func (e *Engine) PreviewBudget(ctx context.Context, chatID, backendID, modelID string) (ContextBudget, error) {
	return e.previewBudget(ctx, chatID, backendID, modelID, nil)
}

// PreviewBudgetWithContext prices a proposed window without changing the chat.
// Zero uses the target model's reported window instead of the old chat override.
func (e *Engine) PreviewBudgetWithContext(ctx context.Context, chatID, backendID, modelID string, numCtx int) (ContextBudget, error) {
	if numCtx < 0 {
		return ContextBudget{}, uimsg.Errorf("chat.contextUnknown")
	}
	return e.previewBudget(ctx, chatID, backendID, modelID, &numCtx)
}

func (e *Engine) previewBudget(ctx context.Context, chatID, backendID, modelID string, numCtx *int) (ContextBudget, error) {
	c, err := e.Store.GetChat(chatID)
	if err != nil {
		return ContextBudget{}, err
	}
	if numCtx != nil {
		c.NumCtx = *numCtx
	}
	settings := e.Settings()

	if backendID == "" {
		backendID = c.Backend
	}
	if modelID == "" {
		modelID = c.Model
	}

	var model llm.ModelInfo
	if backendID != "" && modelID != "" {
		model = e.resolveModelCached(backendID, modelID, settings)
	} else {
		return ContextBudget{}, uimsg.Errorf("model.noneSelected")
	}

	// No provider is needed: this is a preview, and it has to work for a backend
	// the registry does not know.
	assembly, _, err := e.buildAssembly(ctx, &c, backendID, modelID, settings, model, "", "", "")
	if err != nil {
		return ContextBudget{}, err
	}
	return assembly.Budget, nil
}
