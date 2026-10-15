package i18n

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"golang.org/x/text/feature/plural"
	"golang.org/x/text/language"
)

// notPrinted are Russian words ccswap reads rather than prints: answers typed on a Russian layout and
// Anthropic's Russian limit messages.
var notPrinted = map[string]bool{`"д"`: true, `"да"`: true, `"н"`: true, `"т"`: true, `"лимит"`: true, `"сброс"`: true}

var verb = regexp.MustCompile(`%(\[(\d+)\])?[-+# 0]*\d*(\.\d+)?([a-zA-Z%])`)

// verbs maps each argument index a format uses to its verb.
func verbs(f string) map[int]string {
	m, next := map[int]string{}, 1
	for _, v := range verb.FindAllStringSubmatch(f, -1) {
		if v[4] == "%" {
			continue
		}
		if v[2] != "" {
			next, _ = strconv.Atoi(v[2])
		}
		m[next] = v[4]
		next++
	}
	return m
}

func TestVerbs(t *testing.T) {
	if !maps.Equal(verbs("%[2]s resets first, at %[1]s"), verbs("ближайший сброс %s у %s")) ||
		maps.Equal(verbs("%s and %d"), verbs("%d и %s")) || len(verbs("100%%")) != 0 {
		t.Fatal("verbs")
	}
}

func TestEnglishIsComplete(t *testing.T) {
	for _, l := range Available() {
		for k := range catalog[l] {
			if _, ok := catalog[EN][k]; !ok {
				t.Errorf("%s.json: %s is not in en.json", l, k)
			}
		}
	}
}

func TestLocalesHaveEveryKey(t *testing.T) {
	for _, l := range Available() {
		for k := range catalog[EN] {
			if _, ok := catalog[l][k]; !ok {
				t.Errorf("%s.json: %s is missing", l, k)
			}
		}
	}
}

// forms are a message's texts by plural form; a plain message is the form "other" alone. A value of
// another type has none (TestValueTypes reports it).
func forms(v any) map[string]string {
	if s, ok := v.(string); ok {
		return map[string]string{"other": s}
	}
	out := map[string]string{}
	m, _ := v.(map[string]any)
	for f, s := range m {
		out[f], _ = s.(string)
	}
	return out
}

// TestValueTypes: a message is a string, or a plural message: an object of strings.
func TestValueTypes(t *testing.T) {
	for _, l := range Available() {
		for k, v := range catalog[l] {
			m, ok := v.(map[string]any)
			if !ok && !isString(v) || ok && len(m) == 0 {
				t.Errorf("%s.json: %s is %v, not a string or an object of plural forms", l, k, v)
			}
			for f, s := range m {
				if !isString(s) {
					t.Errorf("%s.json: %s: the plural form %s is %v, not a string", l, k, f, s)
				}
			}
		}
	}
}

func TestVerbsMatch(t *testing.T) {
	for _, l := range Available() {
		for k, v := range catalog[l] {
			en, ok := catalog[EN][k]
			if !ok || strings.HasPrefix(k, "web.") { // page messages use {0} placeholders
				continue
			}
			if _, plain := en.(string); plain != isString(v) {
				t.Errorf("%s.json: %s is plural in one language and plain in the other", l, k)
				continue
			}
			want := verbs(forms(en)["other"])
			for f, s := range forms(v) {
				if !maps.Equal(verbs(s), want) {
					t.Errorf("%s.json: %s (%s): verbs of %q differ from English %q", l, k, f, s, forms(en)["other"])
				}
			}
		}
	}
}

func isString(v any) bool { _, ok := v.(string); return ok }

// categories are the plural forms the language's rules pick for whole numbers.
func categories(l Lang) []string {
	tag, seen := language.Make(string(l)), map[string]bool{"other": true}
	for n := range 1000 {
		seen[formName[plural.Cardinal.MatchPlural(tag, n, 0, 0, 0, 0)]] = true
	}
	for _, n := range []int{10000, 100000, 1000000, 10000000} {
		seen[formName[plural.Cardinal.MatchPlural(tag, n, 0, 0, 0, 0)]] = true
	}
	return slices.Sorted(maps.Keys(seen))
}

func TestPluralForms(t *testing.T) {
	plurals := 0
	for _, l := range Available() {
		need := categories(l)
		for k, v := range catalog[l] {
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			plurals++
			for _, f := range need {
				if _, ok := m[f]; !ok {
					t.Errorf("%s.json: %s lacks the plural form %q (%s uses %v)", l, k, f, l, need)
				}
			}
			for f := range m {
				if !slices.Contains(formName[:], f) {
					t.Errorf("%s.json: %s has an unknown plural form %q", l, k, f)
				}
			}
		}
	}
	if plurals == 0 {
		t.Fatal("no plural messages")
	}
}

// TestTags: a file is named by its language's canonical BCP 47 tag (ru, pt-BR, zh-Hant); its lang.name is
// its own; its web.locale, the locale the page shows times in, is a tag of the same language.
func TestTags(t *testing.T) {
	names := map[string]Lang{}
	for _, l := range Available() {
		tag, err := language.Parse(string(l))
		if err != nil || tag.String() != string(l) {
			t.Errorf("%s.json: the name must be a canonical BCP 47 tag (%v %v)", l, tag, err)
		}
		name, _ := catalog[l]["lang.name"].(string)
		if other, ok := names[name]; ok || name == "" {
			t.Errorf("%s.json: lang.name %q is empty or also the name of %s.json", l, name, other)
		}
		names[name] = l
		loc, _ := catalog[l]["web.locale"].(string)
		lt, err := language.Parse(loc)
		if err != nil {
			t.Errorf("%s.json: web.locale %q is not a BCP 47 tag: %v", l, loc, err)
			continue
		}
		lb, _ := lt.Base()
		if fb, _ := tag.Base(); lb != fb {
			t.Errorf("%s.json: web.locale %q is not a %s locale", l, loc, fb)
		}
	}
}

func TestFilesAreCanonical(t *testing.T) {
	names, err := filepath.Glob(filepath.Join("locales", "*.json"))
	if err != nil || len(names) < 2 {
		t.Fatal(names, err)
	}
	for _, name := range names {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(m); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf.Bytes(), b) {
			t.Errorf("%s is not canonical (sorted keys, 2-space indent, trailing newline, UTF-8 without HTML escapes)", name)
		}
	}
}

// goFiles parses every non-test Go file of the module, the generated API aside, by package directory.
func goFiles(t *testing.T, fset *token.FileSet) map[string][]*ast.File {
	root, pkgs := filepath.Join("..", ".."), map[string][]*ast.File{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root && (strings.HasPrefix(d.Name(), ".") || path == filepath.Join(root, "internal", "api")) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		pkgs[filepath.Dir(path)] = append(pkgs[filepath.Dir(path)], f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return pkgs
}

// typed type-checks the packages goFiles finds and calls fn on each file. Imports come from the export data
// `go list -export` leaves in the build cache. No file has a build constraint, so one platform sees them all.
func typed(t *testing.T, fn func(fset *token.FileSet, f *ast.File, info *types.Info)) {
	list := exec.Command("go", "list", "-deps", "-export", "-f", "{{.ImportPath}}={{.Export}}", "./...")
	list.Dir = filepath.Join("..", "..")
	out, err := list.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	exports := map[string]string{}
	for line := range strings.Lines(string(out)) {
		path, file, _ := strings.Cut(strings.TrimSpace(line), "=")
		exports[path] = file
	}
	fset := token.NewFileSet()
	conf := types.Config{Importer: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		return os.Open(exports[path])
	})}
	for dir, files := range goFiles(t, fset) {
		info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{}}
		if _, err := conf.Check(dir, fset, files, info); err != nil {
			t.Fatalf("type-check %s: %v", dir, err)
		}
		for _, f := range files {
			fn(fset, f, info)
		}
	}
}

const pkgPath = "github.com/Dudude-bit/ccswap/internal/i18n"

// keyAt is where the key is among the arguments of i18n's function the call calls; -1 when it calls none of
// T, N, Err and In.
func keyAt(info *types.Info, call *ast.CallExpr) int {
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return -1
	}
	fn, ok := info.Uses[sel.Sel].(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != pkgPath || fn.Signature().Recv() != nil {
		return -1
	}
	switch fn.Name() {
	case "T", "N", "Err":
		return 0
	case "In":
		return 1
	}
	return -1
}

// printfFormat is the index of the format among the arguments of the fmt function the call calls; -1 when
// it calls none that takes one.
func printfFormat(info *types.Info, call *ast.CallExpr) int {
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return -1
	}
	fn, ok := info.Uses[sel.Sel].(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "fmt" {
		return -1
	}
	if i, ok := map[string]int{"Printf": 0, "Sprintf": 0, "Errorf": 0, "Fprintf": 1, "Appendf": 1}[fn.Name()]; ok {
		return i
	}
	return -1
}

var (
	errorType = types.Universe.Lookup("error").Type().Underlying().(*types.Interface)
	stringer  = types.NewInterfaceType([]*types.Func{types.NewFunc(token.NoPos, nil, "String",
		types.NewSignatureType(nil, nil, nil, nil, types.NewTuple(types.NewVar(token.NoPos, nil, "", types.Typ[types.String])), false))}, nil).Complete()
)

func basic(t types.Type, kind types.BasicInfo) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&kind != 0
}

// needs is what verb wants of an argument of type t, "" when t will do; %w wraps in fmt.Errorf only.
func needs(verb string, t types.Type, errorf bool) string {
	switch verb {
	case "v":
		return ""
	case "d", "o":
		if basic(t, types.IsInteger) {
			return ""
		}
		return "an integer"
	case "f":
		if basic(t, types.IsFloat) {
			return ""
		}
		return "a float"
	case "s", "q":
		if basic(t, types.IsString) || types.Implements(t, errorType) || types.Implements(t, stringer) {
			return ""
		}
		return "a string, an error or a Stringer"
	case "w":
		if !errorf {
			return "fmt.Errorf, the only one that wraps"
		}
		if types.Implements(t, errorType) {
			return ""
		}
		return "an error"
	}
	return "a verb this test checks: add it to needs"
}

// checkArgs checks args, the arguments of the text of key, against its verbs: as many as it uses, each of a
// type its verb prints.
func checkArgs(t *testing.T, info *types.Info, pos token.Position, what, key string, args []ast.Expr, errorf bool) {
	text := forms(catalog[EN][key])["other"]
	want := verbs(text)
	if n := slices.Max(append(slices.Collect(maps.Keys(want)), 0)); n != len(args) {
		t.Errorf("%s: %s: %s takes %d arguments, given %d", pos, what, key, n, len(args))
		return
	}
	for i, v := range want {
		if typ := info.TypeOf(args[i-1]); needs(v, typ, errorf) != "" {
			t.Errorf("%s: %s: argument %d of %s is %s, but %%%s needs %s", pos, what, i, key, typ, v, needs(v, typ, errorf))
		}
	}
}

// TestCallSitesUseKnownKeys checks every call of i18n.T, N, Err and In: a constant key that en.json has, of
// the right kind, with the arguments its text's verbs take, of the types they print; a text without
// arguments used as the format of fmt.Printf, Sprintf, Fprintf or Errorf takes that call's arguments. Every
// key outside web.* must be used. The calls are found by their types, so the package may be imported under
// any name, but not with . or _.
func TestCallSitesUseKnownKeys(t *testing.T) {
	used := map[string]bool{"lang.name": true} // read by Name
	calls := 0
	typed(t, func(fset *token.FileSet, f *ast.File, info *types.Info) {
		for _, s := range f.Imports {
			if p, _ := strconv.Unquote(s.Path.Value); p == pkgPath && s.Name != nil && (s.Name.Name == "." || s.Name.Name == "_") {
				t.Errorf("%s: import i18n as %s: call its functions as i18n.T", fset.Position(s.Pos()), s.Name.Name)
			}
		}
		formatOf := map[*ast.CallExpr]*ast.CallExpr{} // a text without arguments → the fmt call it is the format of
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if i := printfFormat(info, call); i >= 0 && i < len(call.Args) {
				if inner, ok := ast.Unparen(call.Args[i]).(*ast.CallExpr); ok {
					formatOf[inner] = call
				}
			}
			at := keyAt(info, call)
			if at < 0 {
				return true
			}
			fn := ast.Unparen(call.Fun).(*ast.SelectorExpr).Sel.Name
			pos := fset.Position(call.Pos())
			calls++
			tv := info.Types[call.Args[at]]
			if tv.Value == nil || tv.Value.Kind() != constant.String {
				t.Errorf("%s: i18n.%s needs a constant key", pos, fn)
				return true
			}
			key := constant.StringVal(tv.Value)
			used[key] = true
			v, ok := catalog[EN][key]
			if !ok {
				t.Errorf("%s: %s is not in en.json", pos, key)
				return true
			}
			if isString(v) == (fn == "N") {
				t.Errorf("%s: i18n.%s cannot take %s: plain and plural messages differ", pos, fn, key)
				return true
			}
			args := call.Args[at+1:] // for N, n is argument 1 of its text
			outer, isFormat := formatOf[call]
			switch {
			case call.Ellipsis.IsValid():
			case fn == "N" && len(verbs(forms(v)["other"])) == 0:
			case fn == "T" && len(args) == 0 && isFormat:
				i := printfFormat(info, outer)
				name := ast.Unparen(outer.Fun).(*ast.SelectorExpr).Sel.Name
				if !outer.Ellipsis.IsValid() {
					checkArgs(t, info, pos, "the format of fmt."+name, key, outer.Args[i+1:], name == "Errorf")
				}
			case len(args) == 0 && strings.Contains(forms(v)["other"], "%"):
				t.Errorf("%s: %s is printed with its %% verbs: give it its arguments or use it as a format", pos, key)
			default:
				checkArgs(t, info, pos, "i18n."+fn, key, args, false)
			}
			return true
		})
	})
	if calls < 100 {
		t.Fatalf("found %d calls: the walk missed the sources", calls)
	}
	for k := range catalog[EN] {
		if !used[k] && !strings.HasPrefix(k, "web.") {
			t.Errorf("en.json: %s is not used", k)
		}
	}
}

func cyrillic(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) })
}

// TestNoRussianInCode: printed texts live in the locale files.
func TestNoRussianInCode(t *testing.T) {
	fset := token.NewFileSet()
	for _, files := range goFiles(t, fset) {
		for _, f := range files {
			ast.Inspect(f, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && cyrillic(lit.Value) && !notPrinted[lit.Value] {
					t.Errorf("%s: Russian outside the locale files: %s", fset.Position(lit.Pos()), lit.Value)
				}
				return true
			})
		}
	}
}
