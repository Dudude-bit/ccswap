package i18n

import (
	"embed"
	"encoding/json"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
)

//go:embed locales/*.json
var files embed.FS

// catalog holds every language's messages: a string, or a plural message as {form: string}.
var catalog = load(files)

func load(fsys fs.FS) map[Lang]map[string]any {
	names, err := fs.Glob(fsys, "locales/*.json")
	if err != nil {
		panic(err)
	}
	out := map[Lang]map[string]any{}
	for _, name := range names {
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			panic(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			panic(name + ": " + err.Error())
		}
		out[Lang(strings.TrimSuffix(path.Base(name), ".json"))] = m
	}
	return out
}

// Available are the languages that have a file, sorted.
func Available() []Lang { return slices.Sorted(maps.Keys(catalog)) }

// Name is the language's name in itself ("Русский").
func Name(l Lang) string {
	if s, ok := catalog[l]["lang.name"].(string); ok {
		return s
	}
	return string(l)
}

// Web is the page's messages (keys under web., without the prefix) in l, English where l has none.
func Web(l Lang) map[string]any {
	out := map[string]any{}
	for _, src := range []Lang{EN, l} {
		for k, v := range catalog[src] {
			if name, ok := strings.CutPrefix(k, "web."); ok {
				out[name] = v
			}
		}
	}
	return out
}
