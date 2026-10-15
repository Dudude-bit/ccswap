// Package i18n prints ccswap's texts in the user's language, from the translation files in locales/.
package i18n

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"

	"golang.org/x/text/language"
)

// Lang is a language: the name of a file in locales/ without .json, a canonical BCP 47 tag (ru, pt-BR).
type Lang string

// EN is the source language: en.json has every key.
const EN Lang = "en"

// current is atomic: the daemon switches language while its goroutines print.
var current atomic.Value

func Set(l Lang) { current.Store(l) }

func Current() Lang {
	if l, ok := current.Load().(Lang); ok {
		return l
	}
	return EN
}

// Pin is a language to pin as config.toml keeps it, its canonical tag ("PT_br": "pt-BR"), or "" for "" (follow
// the locale); false when no file fits it.
func Pin(s string) (string, bool) {
	if s == "" {
		return "", true
	}
	if _, ok := Match(s); !ok {
		return "", false
	}
	return language.Make(s).String(), true
}

// Match is the language whose file fits a BCP 47 tag best: the tag itself, the tag without its last subtags,
// the language in the tag's likely script, then the language alone: pt-BR, then pt; zh-TW, then zh-Hant,
// then zh. The tag may be in any case and use _ (pt_BR).
func Match(tag string) (Lang, bool) {
	t, err := language.Parse(tag)
	if err != nil {
		return "", false
	}
	try := []string{t.String()}
	for s := try[0]; strings.Contains(s, "-"); {
		s = s[:strings.LastIndexByte(s, '-')]
		try = append(try, s)
	}
	// x/text infers zh-TW's script with Low confidence: CLDR's likely subtags, not a guess.
	if sc, c := t.Script(); c != language.No {
		if s := try[len(try)-1] + "-" + sc.String(); !slices.Contains(try, s) {
			try = slices.Insert(try, len(try)-1, s)
		}
	}
	for _, s := range try {
		if _, ok := catalog[Lang(s)]; ok {
			return Lang(s), true
		}
	}
	return "", false
}

// Resolve is the pinned language, else the locale's: the first of LC_ALL, LC_MESSAGES and LANG that is
// set, matched as a tag ("pt_BR" of "pt_BR.UTF-8": pt-BR, else pt); else English.
func Resolve(pinned string, getenv func(string) string) Lang {
	if l, ok := Match(pinned); ok {
		return l
	}
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := getenv(k); v != "" {
			v, _, _ = strings.Cut(v, ".") // the encoding
			v, _, _ = strings.Cut(v, "@") // a modifier
			if l, ok := Match(v); ok {
				return l
			}
			return EN
		}
	}
	return EN
}

// T is the text of key in the current language, formatted with args when there are any.
func T(key string, args ...any) string { return In(Current(), key, args...) }

// In is the text of key in l (English where l has none), formatted with args when there are any; an
// argument that is an *Error is put in l too.
func In(l Lang, key string, args ...any) string {
	v, ok := catalog[l][key]
	if !ok {
		v = catalog[EN][key]
	}
	s, ok := v.(string)
	if !ok {
		return key
	}
	if len(args) == 0 {
		return s
	}
	in := make([]any, len(args))
	for i, a := range args {
		if e, ok := a.(*Error); ok {
			a = e.In(l)
		}
		in[i] = a
	}
	return fmt.Sprintf(s, in...)
}

// N is the plural text of key for the count n: n is argument 1 of the text, args follow it.
func N(key string, n int, args ...any) string {
	l := Current()
	forms, ok := catalog[l][key].(map[string]any)
	if !ok {
		l = EN
		if forms, ok = catalog[l][key].(map[string]any); !ok {
			return key
		}
	}
	s, ok := forms[form(l, n)].(string)
	if !ok {
		s, _ = forms["other"].(string)
	}
	if !strings.Contains(s, "%") {
		return s
	}
	return fmt.Sprintf(s, append([]any{n}, args...)...)
}

// Error picks its text when printed, so a package-level error follows the language set at startup. It also
// serves as a text made now and shown later, such as a notification's reason.
type Error struct {
	key  string
	args []any
}

func Err(key string, args ...any) *Error { return &Error{key, args} }

func (e *Error) Error() string { return e.In(Current()) }

// In is the text in l.
func (e *Error) In(l Lang) string { return In(l, e.key, e.args...) }

// LogValue puts the text in English: logs (daemon.log) stay English whatever language ccswap speaks.
func (e *Error) LogValue() slog.Value { return slog.StringValue(e.In(EN)) }

// Unwrap is the first argument that is an error, as %w would wrap it: Err("k", cause) instead of
// fmt.Errorf("…: %w", cause), whose text would keep the language of the moment it was made.
func (e *Error) Unwrap() error {
	for _, a := range e.args {
		if err, ok := a.(error); ok {
			return err
		}
	}
	return nil
}
