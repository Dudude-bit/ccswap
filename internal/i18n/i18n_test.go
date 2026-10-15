package i18n

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/fstest"
)

const RU Lang = "ru"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestResolve(t *testing.T) {
	for _, c := range []struct {
		pinned string
		env    map[string]string
		want   Lang
	}{
		{"", nil, EN},
		{"", map[string]string{"LANG": "ru_RU.UTF-8"}, RU},
		{"", map[string]string{"LANG": "en_US.UTF-8"}, EN},
		{"", map[string]string{"LANG": "qaa_QA.UTF-8"}, EN},
		{"", map[string]string{"LC_ALL": "ru_RU.UTF-8"}, RU},
		{"", map[string]string{"LANG": "ru_RU.UTF-8", "LC_MESSAGES": "en_US.UTF-8"}, EN},
		{"", map[string]string{"LANG": "en_US.UTF-8", "LC_MESSAGES": "ru"}, RU},
		{"", map[string]string{"LANG": "en_US.UTF-8", "LC_ALL": "ru_RU.UTF-8", "LC_MESSAGES": "en_US.UTF-8"}, RU},
		{"", map[string]string{"LC_ALL": "C", "LANG": "ru_RU.UTF-8"}, EN},
		{"ru", map[string]string{"LANG": "en_US.UTF-8"}, RU},
		{"en", map[string]string{"LC_ALL": "ru_RU.UTF-8"}, EN},
		{"qaa", map[string]string{"LANG": "ru_RU.UTF-8"}, RU},
		{"xx", map[string]string{"LANG": "ru_RU.UTF-8"}, RU},
	} {
		if got := Resolve(c.pinned, env(c.env)); got != c.want {
			t.Errorf("Resolve(%q, %v) = %s, want %s", c.pinned, c.env, got, c.want)
		}
	}
	for s, want := range map[string]string{"": "", "ru": "ru", "en": "en", "RU": "ru", "ru_ru": "ru-RU", "xx": "!", "qaa": "!", "auto": "!"} {
		if got, ok := Pin(s); got != want && ok || ok != (want != "!") {
			t.Errorf("Pin(%q) = %q %v, want %q", s, got, ok, want)
		}
	}
}

func TestMatch(t *testing.T) {
	withCatalog(t, map[string]string{"en.json": `{}`, "pt.json": `{}`, "pt-BR.json": `{}`, "zh-Hant.json": `{}`, "zh.json": `{}`})
	for tag, want := range map[string]Lang{
		"pt-BR": "pt-BR", "pt_BR": "pt-BR", "PT-br": "pt-BR", "pt-PT": "pt", "pt": "pt", "zh-Hant-TW": "zh-Hant",
		"zh-TW": "zh-Hant", "zh-HK": "zh-Hant", "zh_MO": "zh-Hant", "zh-CN": "zh", "zh-Hans": "zh", "zh": "zh",
		"en-US": EN, "de": "", "C": "", "": "", "auto": "",
	} {
		if got, ok := Match(tag); got != want || ok != (want != "") {
			t.Errorf("Match(%q) = %q %v, want %q", tag, got, ok, want)
		}
	}
	for _, c := range []struct {
		pinned string
		env    map[string]string
		want   Lang
	}{
		{"", map[string]string{"LANG": "pt_BR.UTF-8"}, "pt-BR"},
		{"", map[string]string{"LANG": "pt_PT.UTF-8"}, "pt"},
		{"", map[string]string{"LC_ALL": "zh_Hant_TW.UTF-8@x"}, "zh-Hant"},
		{"", map[string]string{"LANG": "zh_TW.UTF-8"}, "zh-Hant"},
		{"", map[string]string{"LANG": "de_DE.UTF-8"}, EN},
		{"pt_BR", map[string]string{"LANG": "en_US.UTF-8"}, "pt-BR"},
		{"pt-AO", nil, "pt"},
	} {
		if got := Resolve(c.pinned, env(c.env)); got != c.want {
			t.Errorf("Resolve(%q, %v) = %s, want %s", c.pinned, c.env, got, c.want)
		}
	}
	for s, want := range map[string]string{"PT_br": "pt-BR", "pt-ao": "pt-AO", "zh_TW": "zh-TW", "de": "!"} {
		if got, ok := Pin(s); got != want && ok || ok != (want != "!") {
			t.Errorf("Pin(%q) = %q %v, want %q: a pin is a tag a file fits, kept canonical", s, got, ok, want)
		}
	}
}

func TestRussianPlural(t *testing.T) {
	t.Cleanup(func() { Set(EN) })
	Set(RU)
	for n, want := range map[int]string{1: "1 аккаунт", 2: "2 аккаунта", 5: "5 аккаунтов", 11: "11 аккаунтов",
		21: "21 аккаунт", 22: "22 аккаунта", 25: "25 аккаунтов", 0: "0 аккаунтов", 111: "111 аккаунтов"} {
		if got := N("list.accounts", n); got != want {
			t.Errorf("N(list.accounts, %d) = %q, want %q", n, got, want)
		}
	}
	Set(EN)
	if got := N("list.accounts", 1) + ", " + N("list.accounts", 2); got != "1 account, 2 accounts" {
		t.Errorf("English plural: %q", got)
	}
}

// withCatalog swaps in a catalog built from the given files for one test.
func withCatalog(t *testing.T, files map[string]string) {
	fsys := fstest.MapFS{}
	for name, data := range files {
		fsys["locales/"+name] = &fstest.MapFile{Data: []byte(data)}
	}
	old := catalog
	catalog = load(fsys)
	t.Cleanup(func() { catalog = old; Set(EN) })
}

func TestMissingTranslationFallsBack(t *testing.T) {
	withCatalog(t, map[string]string{
		"en.json": `{"x.both": "both %d", "x.only_en": "only English %s", "x.n": {"one": "%d file", "other": "%d files"}}`,
		"ru.json": `{"x.both": "оба %d"}`,
	})
	Set(RU)
	if got := T("x.both", 2); got != "оба 2" {
		t.Errorf("translated: %q", got)
	}
	if got := T("x.only_en", "here"); got != "only English here" {
		t.Errorf("missing in ru: %q", got)
	}
	if got := N("x.n", 1) + ", " + N("x.n", 3); got != "1 file, 3 files" {
		t.Errorf("missing plural in ru: %q", got)
	}
	if got := Err("x.only_en", "e").Error(); got != "only English e" {
		t.Errorf("missing error in ru: %q", got)
	}
	if got := T("x.nowhere") + ", " + N("x.nowhere", 2); got != "x.nowhere, x.nowhere" {
		t.Errorf("a key no language has: %q", got)
	}
}

func TestErrIdentity(t *testing.T) {
	withCatalog(t, map[string]string{"en.json": `{"x.down": "daemon is down"}`, "ru.json": `{"x.down": "демон не запущен"}`})
	e := Err("x.down")
	if !errors.Is(fmt.Errorf("w: %w", e), e) || errors.Is(e, Err("x.down")) {
		t.Fatal("the error must keep its identity")
	}
	joined := errors.Join(e) // reads e.Error() when printed, not when made
	Set(RU)
	if joined.Error() != "демон не запущен" {
		t.Fatalf("Russian not picked: %q", joined.Error())
	}
	Set(EN)
	if joined.Error() != "daemon is down" {
		t.Fatalf("English not picked: %q", joined.Error())
	}
}

func TestAvailable(t *testing.T) {
	got := Available()
	if !slices.IsSorted(got) || !slices.Contains(got, EN) || !slices.Contains(got, RU) {
		t.Fatalf("Available() = %v", got)
	}
	if Name(EN) != "English" || Name(RU) != "Русский" {
		t.Fatalf("Name: %q %q", Name(EN), Name(RU))
	}
	withCatalog(t, map[string]string{"en.json": `{"web.title": "Limits", "web.hint": "Hi", "x.k": "x"}`, "ru.json": `{"web.title": "Лимиты"}`})
	if w := Web(RU); len(w) != 2 || w["title"] != "Лимиты" || w["hint"] != "Hi" {
		t.Fatalf("Web(ru) = %v", w)
	}
}

func TestIn(t *testing.T) {
	withCatalog(t, map[string]string{
		"en.json": `{"x.switched": "Switched to %s: %s", "x.hit": "%s hit its limit"}`,
		"ru.json": `{"x.switched": "Переключил на %s: %s", "x.hit": "кончился лимит у %s"}`,
	})
	Set(RU)
	msg := Err("x.switched", "b", Err("x.hit", "a")) // the reason follows the language of the message
	if got := msg.Error(); got != "Переключил на b: кончился лимит у a" {
		t.Errorf("Error: %q", got)
	}
	if got := msg.LogValue().String(); got != "Switched to b: a hit its limit" {
		t.Errorf("LogValue: %q", got)
	}
	if got := In(EN, "x.switched", "b", Err("x.hit", "a")) + ", " + In(RU, "x.hit", "c"); got != "Switched to b: a hit its limit, кончился лимит у c" {
		t.Errorf("In: %q", got)
	}
	cause := Err("x.hit", "a") // wrapped as an argument, as %w would: errors.Is finds it, the text stays lazy
	if w := Err("x.switched", "b", cause); !errors.Is(w, cause) || errors.Unwrap(w) != cause || errors.Unwrap(Err("x.hit", "a")) != nil {
		t.Error("Unwrap is the first argument that is an error")
	}
}
