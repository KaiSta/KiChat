package chat

import "kichat/internal/config"

// promptLanguage resolves the language of a background model prompt from the
// language the user set for the app. It is deliberately the setting and not a
// guess at the conversation's language: the user chose this one for the interface,
// and a detector's mistake would rewrite every memory and every condensation in
// the wrong language. German and Chinese have contracts of their own; anything
// else gets English.
func promptLanguage(ui string) string {
	switch ui {
	case config.LangGerman, config.LangChinese:
		return ui
	default:
		return config.LangEnglish
	}
}
