// Package lang answers the one question every package that writes a sentence for a person or
// for a model has to ask: which of two wordings is this text in.
//
// It exists as its own package rather than as a helper in internal/config because of an import
// cycle, and the cycle is real rather than an accident: internal/config carries the image
// settings, so it imports internal/images, and internal/images writes sentences of its own. A
// leaf package with no imports is the only place both sides can reach.
//
// What it is *not* is a translation table. The wordings live at their call sites, next to each
// other, so a reader changing one rule changes both copies of it in one place — a refusal that
// contradicts itself across two languages is worse than one that is merely untranslated.
package lang

// The interface languages this app ships. They are the same strings config uses
// (config.LangGerman and friends); the constant is repeated here because the leaf package
// cannot import the one that defines them.
const (
	German  = "de"
	English = "en"
)

// Resolve answers the language model-facing text is written in: the interface's language
// where a translation exists, which today means German for a German window and English for
// everything else — including a Chinese one, the same fallback config.PromptBody uses for an
// untranslated prompt, because a sentence nobody wrote is worse than one in the wrong language.
//
// What a model is *told* therefore follows the person reading the window.
func Resolve(l string) string {
	if l == German {
		return German
	}
	return English
}

// Text picks the sentence for a resolved language (Resolve). Anything other than German gets
// the English wording, so an unknown value cannot silently produce German.
func Text(resolved, de, en string) string {
	if resolved == German {
		return de
	}
	return en
}

// Of is Text's companion for a package that carries a language *supplier* rather than a
// string: settings can be saved while a turn is running, so a service that stored the value at
// construction would keep answering in the old language until a restart.
//
// A nil supplier is English — config.Default()'s language, and what a package built by a test
// speaks.
func Of(supplier func() string) string {
	if supplier == nil {
		return English
	}
	return Resolve(supplier())
}
