package i18n

import (
	"golang.org/x/text/feature/plural"
	"golang.org/x/text/language"
)

// formName is the CLDR name of each plural.Form.
var formName = [...]string{plural.Other: "other", plural.Zero: "zero", plural.One: "one", plural.Two: "two", plural.Few: "few", plural.Many: "many"}

// form is the CLDR plural form l uses for the whole number n.
func form(l Lang, n int) string {
	if n < 0 {
		n = -n
	}
	return formName[plural.Cardinal.MatchPlural(language.Make(string(l)), n, 0, 0, 0, 0)]
}
