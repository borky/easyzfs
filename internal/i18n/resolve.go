package i18n

// Resolve — the language to write to a user in: their explicit choice
// ("es"/"en"), else the language their UI was last seen showing (uiLang,
// recorded from their requests), else fallback, else English — the
// product's language.
func Resolve(language, uiLang, fallback string) string {
	for _, l := range []string{language, uiLang, fallback} {
		if l == "es" || l == "en" {
			return l
		}
	}
	return "en"
}
