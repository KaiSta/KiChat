package chat

import (
	"context"
	"fmt"
	"strings"

	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/llm"
	"kichat/internal/store"
)

// A request a backend refuses for its own size used to end the turn.
//
// That is the wrong answer for the failure, and it is worst on Groq, where the
// refusal is the ordinary consequence of a long chat rather than a mistake: the
// account's per-minute token allowance is also the largest single request it will
// accept, so a conversation planned against gpt-oss-20b's 131k window — which is
// what the context meter shows, because that is the window — is refused outright
// once it passes a few thousand tokens. The vendor's answer is explicit about the
// fix ("please reduce your message size and try again"), and the app already knows
// how to reduce a message size: it folds history into a memory. Nothing did that,
// so the user got a red row and a 413 body, and the only way forward was to edit or
// abandon the chat.
//
// Two things happen here instead:
//
//   - the refusal is folded and the turn is sent once more (foldForSize), so an
//     over-long chat continues instead of aborting, and the user is told why in one
//     sentence naming the limit the backend named;
//   - once a ceiling is known, the next request is planned against it before it goes
//     out (foldForCeiling), so the refusal is paid once per model per session rather
//     than once per turn.
//
// Everything here is gated on a refusal or on the backend's own published number.
// No threshold is invented for a backend that reports none: a chat is never folded
// because the app guessed that a request was too big, only because the backend said
// so or because it published a limit the request would cross.

// ceilingFoldRatio is how close to a published ceiling a request may come before the
// history is folded. It is below 1 because the meter's number is partly an estimate
// (the token counter is a heuristic, deliberately — see llm.EstimateTokens) while the
// ceiling is exact, and undershooting the fold point costs one summarisation while
// overshooting it costs a refused turn.
const ceilingFoldRatio = 0.9

// sizeTarget is the fraction of the chat's own window that a folded prompt should
// fit, which is how a ceiling that is not the window is handed to the planner: the
// plan prices the kept turns against Budget.Limit, so aiming at ceiling/window aims
// the folded request at the ceiling itself.
//
// With no ceiling the request was over the model's *window*, so the aim is the same
// ratio the automatic threshold uses — the user's setting, and 0.8 when they have
// not touched it.
func sizeTarget(chat store.Chat, settings config.Settings, modelInfo llm.ModelInfo, ceiling int) float64 {
	limit, _ := limitFor(chat, modelInfo)
	if limit <= 0 {
		// The planner's target branch needs a limit to price against; without one it
		// folds as much as it can, which is the best available answer.
		return 0
	}
	if ceiling <= 0 {
		return effectiveThreshold(chat, settings)
	}
	return clampRatio(float64(ceiling) / float64(limit))
}

// clampRatio keeps a target inside the band the planner can act on.
//
// The floor is tiny and still necessary, and the reason is the planner's own
// convention: `Target: 0` means "fold as much as your cut allows" and *skips* the
// target walk entirely, so a ceiling so small that the ratio rounded to zero would
// silently fold only to the keep-turns boundary — the one thing a size fold exists to
// go past when the limit demands it. Above that, the walk's own limits are the bound:
// it never folds the turn that started the request, however small the target is. The
// cap keeps a ceiling larger than the window from asking the planner for no reduction
// at all.
func clampRatio(ratio float64) float64 {
	switch {
	case ratio < 0.001:
		return 0.001
	case ratio > 0.95:
		return 0.95
	default:
		return ratio
	}
}

// ceilingNote names the limit the refusal stated, for the sentence the user reads.
// It is deliberately about the number and not about where it came from: the user is
// being told why their message did not fit, not how the app learned the limit.
func ceilingNote(too *llm.RequestTooLargeError) string {
	if too == nil || too.Limit <= 0 {
		return ""
	}
	if too.Requested > 0 {
		return fmt.Sprintf(" (höchstens %d Token pro Anfrage, angefragt waren %d)",
			too.Limit, too.Requested)
	}
	return fmt.Sprintf(" (höchstens %d Token pro Anfrage)", too.Limit)
}

// foldForSize folds a chat's history because the backend refused the request for its
// own size. It reports whether a memory was written, which is also whether the
// request is worth sending again: a fold that had nothing to fold would produce the
// identical body and the identical refusal.
//
// The fold is forced, deliberately. settings.Compact.Enabled and the chat's
// AutoCompact decide whether compaction happens *quietly*; this is the only way
// forward for a turn the backend has already refused, which puts it beside the
// Compact button rather than beside the silent threshold — a run that happens
// because something asked. Its card says `too_large` and not `manual`, because what
// asked was the backend.
func (e *Engine) foldForSize(ctx context.Context, chat *store.Chat, provider llm.Provider,
	backendID, modelID string, modelInfo llm.ModelInfo, settings config.Settings,
	too *llm.RequestTooLargeError) bool {

	// The refused request was never answered, so the empty reply row it left behind
	// goes: the retry uses the same user message, and a red row in the transcript for
	// a request nobody answered reads as a broken chat rather than as a recovery.
	if err := e.dropTrailingAssistant(chat.ID); err != nil {
		e.persist(chat.ID, "Die fehlgeschlagene Antwort", err)
	}

	// The refusal is announced first, and it is the only part that is certain: the
	// limit named in it is what the user needs to act on if nothing can be folded.
	e.emit(events.Warning, map[string]any{
		"chatId": chat.ID,
		"warning": fmt.Sprintf("%s hat die Anfrage als zu groß abgelehnt%s.",
			provider.Name(), ceilingNote(too)),
	})

	compaction, err := e.compactWith(ctx, chat, provider, backendID, modelID, modelInfo, settings,
		CompactOptions{
			Force:             true,
			Target:            sizeTarget(*chat, settings, modelInfo, too.Limit),
			Trigger:           triggerTooLarge,
			ProtectNewestTurn: true,
		})
	if err == nil && compaction != nil {
		// Said only once the fold has happened: a promise of a retry that never comes
		// would be the one thing in this turn that is not true.
		e.emit(events.Warning, map[string]any{
			"chatId":  chat.ID,
			"warning": "Der Verlauf wurde komprimiert — die Anfrage wird erneut gesendet.",
		})
		return true
	}
	// Folding is best-effort. A summariser that fails — refused for its own size, no
	// backend, no room to summarise in — must not replace a refusal the user can act
	// on with a different error they cannot, so it is reported and the refusal stands.
	if err != nil {
		e.emit(events.Warning, map[string]any{
			"chatId":  chat.ID,
			"warning": fmt.Sprintf("Der Verlauf konnte nicht komprimiert werden: %s", err.Error()),
		})
	}
	return false
}

// foldForCeiling folds the history *before* the request goes out, when the backend
// has published a ceiling for this model and the next request would run into it.
//
// It is the same fold as foldForSize, moved earlier: once the ceiling is known
// (llm.RequestCeiling), every request after that is planned against the real limit
// instead of being refused against it. Only a backend that publishes one is affected
// — Groq — so nothing here changes for a backend that reports only a window.
//
// What the ceiling is learned from is the difference between the first oversized
// request of a session being sent and being prevented, and there are two sources: the
// budget header of any response (an ordinary answer is enough — see
// openAICompatProvider.Chat) and a refusal's own sentence, which is what a session
// whose very first request is oversized still pays once.
func (e *Engine) foldForCeiling(ctx context.Context, chat *store.Chat, provider llm.Provider,
	backendID, modelID string, modelInfo llm.ModelInfo, settings config.Settings) (bool, error) {

	ceiler, ok := provider.(llm.RequestCeiling)
	if !ok {
		return false, nil
	}
	ceiling := ceiler.RequestCeiling(modelID).Tokens
	if ceiling <= 0 {
		return false, nil
	}
	assembly, _, err := e.buildAssembly(ctx, chat, backendID, modelID, settings, modelInfo, "")
	if err != nil {
		return false, err
	}
	// Projected rather than Used, because the reserve for the answer is part of what
	// goes out with the request and the ceiling governs the whole request.
	projected := assembly.Budget.Projected
	if projected <= 0 {
		projected = assembly.Budget.Used
	}
	if float64(projected) < float64(ceiling)*ceilingFoldRatio {
		return false, nil
	}
	// The trigger stays "auto": a threshold was crossed and nobody pressed
	// anything, which is exactly what that name means. What it was measured against
	// is this backend's published ceiling rather than its window.
	compaction, err := e.compactWith(ctx, chat, provider, backendID, modelID, modelInfo, settings,
		CompactOptions{
			Force:             true,
			Target:            sizeTarget(*chat, settings, modelInfo, ceiling),
			Trigger:           triggerAuto,
			ProtectNewestTurn: true,
		})
	if err != nil {
		return false, err
	}
	return compaction != nil, nil
}

// turnFailureText is what the user is shown for a failed turn.
//
// A refusal caused by the request's own size is the one failure whose message is
// worth replacing. The vendor's sentence is written for an API client — it names an
// organization, a service tier and a billing page — while what the user needs is
// which limit was hit, whether the app already tried to fit it, and what reduces it.
// Every other failure keeps the backend's own words, which is the rule everywhere
// else in this engine.
func turnFailureText(providerName string, err error, folded bool) string {
	too, ok := llm.RequestTooLarge(err)
	if !ok {
		return err.Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Die Anfrage ist zu groß für %s%s", providerName, ceilingNote(too))
	if folded {
		b.WriteString(" — auch mit komprimiertem Verlauf passt sie nicht hinein.")
	} else {
		b.WriteString(" — es gab keinen Verlauf, der sich noch hätte zusammenfassen lassen.")
	}
	b.WriteString(" Kürze die Nachricht, entferne Anhänge oder wähle ein Modell mit einem höheren Limit.")
	return b.String()
}
