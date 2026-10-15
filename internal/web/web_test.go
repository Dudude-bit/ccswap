package web

import (
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/gofiber/fiber/v3"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/i18n"
)

func TestPageFiles(t *testing.T) {
	app := fiber.New()
	Register(app)
	page := ""
	for _, c := range []struct{ path, kind string }{
		{Page, "text/html"}, {"/app.css", "text/css"}, {"/app.js", "javascript"}, {"/i18n.js", "javascript"},
	} {
		resp, err := app.Test(httptest.NewRequest("GET", c.path, nil))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), c.kind) || resp.Header.Get("Content-Security-Policy") == "" || len(b) == 0 {
			t.Fatalf("%s: %d %q csp=%q %d bytes", c.path, resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Security-Policy"), len(b))
		}
		if c.path == Page {
			page = string(b)
		}
	}
	for _, src := range []string{`src="i18n.js"`, `src="app.js"`} {
		if !strings.Contains(page, src) {
			t.Fatalf("the page does not load %s", src)
		}
	}
}

var (
	pageKey   = regexp.MustCompile(`data-t(?:-label|-ph|-title)?="([^"]+)"`)
	callKey   = regexp.MustCompile(`\b(?:t|tNodes)\('([^']+)'[,)]`)
	prefixKey = regexp.MustCompile(`\b(?:t|tNodes)\('([^']+\.)' \+`)
	codeList  = regexp.MustCompile(`(?s)const (ERR_CODES|REASONS) = \[(.*?)\];`)
	dottedKey = regexp.MustCompile(`'([a-z][A-Za-z0-9_]*(?:\.[A-Za-z0-9_]+)+)'`)
	quoted    = regexp.MustCompile(`'([^'\n]+)'`)
	entry     = regexp.MustCompile(`(?m)(?:^|[{,])\s*'[a-z][A-Za-z0-9_]*\.[A-Za-z0-9_.]+':`)
	marks     = regexp.MustCompile(`\*\*|` + "`")
	holder    = regexp.MustCompile(`\{\d+\}`)
	scripts   = []string{"app.js", "i18n.js"}
)

func read(t *testing.T, name string) string {
	t.Helper()
	b, err := files.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// lists are the code lists of app.js, by the key prefix the page adds their codes to: ERR_CODES (e.<code>)
// and REASONS (r.<reason>).
func lists(t *testing.T) map[string][]string {
	out := map[string][]string{}
	for _, m := range codeList.FindAllStringSubmatch(read(t, "app.js"), -1) {
		p := map[string]string{"ERR_CODES": "e.", "REASONS": "r."}[m[1]]
		for _, q := range quoted.FindAllStringSubmatch(m[2], -1) {
			out[p] = append(out[p], q[1])
		}
	}
	if len(out["e."]) == 0 || len(out["r."]) == 0 {
		t.Fatalf("app.js lists no ERR_CODES or REASONS: %v", out)
	}
	return out
}

// TestPageKeysExist: every key the page names is under web. in en.json, and every web. key is named. The
// page names a key in a data-t* attribute, as a t('…') or tNodes('…') argument, as a quoted 'area.name'
// anywhere in its scripts, or as a code of ERR_CODES or REASONS that it adds to e. or r. (t('e.' + code));
// a prefix with no such list fails.
func TestPageKeysExist(t *testing.T) {
	en := i18n.Web(i18n.EN)
	if len(en) < 100 {
		t.Fatalf("only %d page messages", len(en))
	}
	named := map[string]bool{}
	for _, m := range pageKey.FindAllStringSubmatch(read(t, "index.html"), -1) {
		named[m[1]] = true
	}
	quotes := map[string]bool{}
	codes := lists(t)
	for _, name := range scripts {
		src := read(t, name)
		for _, re := range []*regexp.Regexp{callKey, dottedKey} {
			for _, m := range re.FindAllStringSubmatch(src, -1) {
				named[m[1]] = true
			}
		}
		for _, m := range prefixKey.FindAllStringSubmatch(src, -1) {
			if codes[m[1]] == nil {
				t.Errorf("%s adds codes to %s without a list of them", name, m[1])
			}
		}
		for _, m := range quoted.FindAllStringSubmatch(src, -1) {
			quotes[m[1]] = true
		}
	}
	for p, list := range codes {
		for _, c := range list {
			named[p+c] = true
		}
	}
	for k := range named {
		if _, ok := en[k]; !ok {
			t.Errorf("the page uses %s, which en.json lacks as web.%s", k, k)
		}
	}
	for k := range en {
		if !named[k] && !quotes[k] {
			t.Errorf("en.json: web.%s is not used by the page", k)
		}
	}
}

// pageCodes are the error codes the page makes itself (api() in app.js).
var pageCodes = []string{"offline", "timeout"}

// notShown are the API's error codes the page has no text of its own for, and why.
var notShown = map[string]string{
	"bad_request":        "the page sends well-formed requests: e.other",
	"invalid_strategy":   "the page offers the valid strategies only: e.other",
	"unauthorized":       "the login screen",
	"near_refresh":       "x.near, with the way out",
	"invalid_name":       "import and adopt are on the unix socket",
	"invalid_lang":       "the unix socket only",
	"invalid_remote":     "the unix socket only",
	"invalid_config_dir": "the unix socket only",
	"login_incomplete":   "the unix socket only",
	"no_entry":           "the unix socket only",
	"duplicate":          "the unix socket only",
	"remove_active":      "the unix socket only",
	"active_account":     "the unix socket only",
	"migrate_active":     "migrate is on the unix socket only",
	"export_gone":        "migrate is on the unix socket only",
	"export_done":        "migrate is on the unix socket only",
	"tokens_changed":     "migrate is on the unix socket only",
	"not_bundle":         "migrate is on the unix socket only",
	"bundle_version":     "migrate is on the unix socket only",
	"invalid_code":       "migrate is on the unix socket only",
	"bad_bundle":         "migrate is on the unix socket only",
	"bundle_spent":       "migrate is on the unix socket only",
	"bundle_unchecked":   "migrate is on the unix socket only",
}

// apiCodes are the error codes api/openapi.yaml lists in the description of Error.code.
func apiCodes(t *testing.T) []string {
	sw, err := api.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	desc := sw.Components.Schemas["Error"].Value.Properties["code"].Value.Description
	_, list, _ := strings.Cut(desc, "\n")
	list = regexp.MustCompile(`\([^)]*\)`).ReplaceAllString(list, "")
	codes := regexp.MustCompile(`[a-z][a-z_]*`).FindAllString(list, -1)
	if len(codes) < 20 {
		t.Fatalf("only %d codes in %q", len(codes), list)
	}
	return codes
}

// TestErrorCodes: ERR_CODES are the codes with an e.<code> text: the API's and the page's own; every
// other code the API has is in notShown, with its reason.
func TestErrorCodes(t *testing.T) {
	all, page := apiCodes(t), lists(t)["e."]
	en := i18n.Web(i18n.EN)
	for _, c := range page {
		if !slices.Contains(all, c) && !slices.Contains(pageCodes, c) || notShown[c] != "" {
			t.Errorf("ERR_CODES has %s, which is neither an API code nor the page's own, or is in notShown", c)
		}
	}
	for _, c := range all {
		if !slices.Contains(page, c) && notShown[c] == "" {
			t.Errorf("the API has the error code %s: give it a text (ERR_CODES, e.%s) or put it in notShown", c, c)
		}
	}
	for c := range notShown {
		if !slices.Contains(all, c) {
			t.Errorf("notShown has %s, which api/openapi.yaml does not list", c)
		}
	}
	for k := range en {
		if c, ok := strings.CutPrefix(k, "e."); ok && c != "other" && !slices.Contains(page, c) {
			t.Errorf("web.%s has no code in ERR_CODES", k)
		}
		if r, ok := strings.CutPrefix(k, "r."); ok && r != "model" && r != "default" && !slices.Contains(lists(t)["r."], r) {
			t.Errorf("web.%s has no reason in REASONS", k)
		}
	}
}

// forms are a page message's texts by plural form; a plain message is the form "other" alone. A value of
// another type has none (TestValueTypes in internal/i18n reports it).
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

// shape is what a translation must keep of English: how often each placeholder and each kind of mark
// occurs; word order may move them.
func shape(s string) string {
	h, m := holder.FindAllString(s, -1), marks.FindAllString(s, -1)
	slices.Sort(h)
	slices.Sort(m)
	return strings.Join(h, "") + " " + strings.Join(m, "")
}

// TestPagePlaceholdersMatch: in every language each page message has English's {n} placeholders, each as
// often as English, and as many ** and ` marks, which come in pairs; plain stays plain and plural stays plural.
func TestPagePlaceholdersMatch(t *testing.T) {
	en := i18n.Web(i18n.EN)
	for k, v := range en {
		for f, s := range forms(v) {
			if strings.Count(s, "**")%2 != 0 || strings.Count(s, "`")%2 != 0 {
				t.Errorf("en.json: web.%s (%s): unpaired marks in %q", k, f, s)
			}
		}
	}
	for _, l := range i18n.Available() {
		for k, v := range i18n.Web(l) {
			ev, ok := en[k]
			if !ok {
				continue // TestEnglishIsComplete in internal/i18n
			}
			if _, plain := ev.(string); plain != isString(v) {
				t.Errorf("%s.json: web.%s is plural in one language and plain in the other", l, k)
				continue
			}
			want := forms(ev)["other"]
			for f, s := range forms(v) {
				if shape(s) != shape(want) {
					t.Errorf("%s.json: web.%s (%s): %q does not keep the placeholders and marks of %q", l, k, f, s, want)
				}
			}
		}
	}
}

func isString(v any) bool { _, ok := v.(string); return ok }

// TestNoDictionaryInJS: the page's texts come from the daemon, so its scripts hold none.
func TestNoDictionaryInJS(t *testing.T) {
	en := i18n.Web(i18n.EN)
	for _, name := range scripts {
		src := read(t, name)
		if strings.ContainsFunc(src, func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) }) {
			t.Errorf("%s has Cyrillic text", name)
		}
		if m := entry.FindString(src); m != "" {
			t.Errorf("%s has a dictionary entry: %s", name, m)
		}
		for k, v := range en {
			if s, ok := v.(string); ok && len(s) >= 15 && strings.Contains(src, s) {
				t.Errorf("%s holds the text of web.%s", name, k)
			}
		}
	}
}

// TestScriptsInNode runs i18n.test.js, the page's i18n.js on the real texts, when node is installed;
// scripts/linux-test.sh runs it in a node container.
func TestScriptsInNode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
	if b, err := exec.Command("node", "--test", "--test-timeout=30000", "i18n.test.js").CombinedOutput(); err != nil {
		t.Fatalf("node --test i18n.test.js: %v\n%s", err, b)
	}
}

func TestFonts(t *testing.T) {
	app := fiber.New()
	Register(app)
	for p, kind := range map[string]string{
		"/fonts/geologica.woff2": "font/woff2", "/fonts/jetbrains-mono.woff2": "font/woff2",
		"/fonts/OFL-Geologica.txt": "text/plain; charset=utf-8", "/fonts/OFL-JetBrainsMono.txt": "text/plain; charset=utf-8",
		Page: "text/html; charset=utf-8",
	} {
		resp, err := app.Test(httptest.NewRequest("GET", p, nil))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != kind || len(b) == 0 || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s: %d %q %d bytes", p, resp.StatusCode, resp.Header.Get("Content-Type"), len(b))
		}
		if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "font-src 'self'") {
			t.Fatalf("%s: CSP lacks font-src 'self'", p)
		}
		if strings.HasSuffix(p, ".txt") && !strings.Contains(string(b), "SIL OPEN FONT LICENSE") {
			t.Fatalf("%s: not the OFL text", p)
		}
	}
}

// TestFontsDrawEveryText: both fonts draw every character of the page's texts, by the code points
// scripts/fonts.sh lists beside them; a character they lack comes from a system font.
func TestFontsDrawEveryText(t *testing.T) {
	for _, font := range []string{"geologica", "jetbrains-mono"} {
		b, err := os.ReadFile("fonts/" + font + ".cmap")
		if err != nil {
			t.Fatal(err)
		}
		var drawn [][2]uint64
		for _, line := range strings.Fields(string(b)) {
			lo, hi, ok := strings.Cut(line, "-")
			if !ok {
				hi = lo
			}
			l, err1 := strconv.ParseUint(lo, 16, 32)
			h, err2 := strconv.ParseUint(hi, 16, 32)
			if err1 != nil || err2 != nil {
				t.Fatalf("%s.cmap: %q", font, line)
			}
			drawn = append(drawn, [2]uint64{l, h})
		}
		for _, l := range i18n.Available() {
			texts := map[string]string{"lang.name": i18n.Name(l)}
			for k, v := range i18n.Web(l) {
				for f, s := range forms(v) {
					texts["web."+k+" ("+f+")"] = s
				}
			}
			for k, s := range texts {
				for _, r := range s {
					if !slices.ContainsFunc(drawn, func(d [2]uint64) bool { return d[0] <= uint64(r) && uint64(r) <= d[1] }) {
						t.Errorf("%s.json: %s has %q (U+%04X), which %s lacks: add it to UNI in scripts/fonts.sh", l, k, r, r, font)
					}
				}
			}
		}
	}
}

// TestStylesheetBraces: app.css closes every block it opens. A stray brace makes browsers drop the rules
// after it, such as a whole media query, without an error anywhere.
func TestStylesheetBraces(t *testing.T) {
	depth := 0
	for i, line := range strings.Split(read(t, "app.css"), "\n") {
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if depth < 0 {
			t.Fatalf("app.css:%d closes a block it never opened", i+1)
		}
	}
	if depth != 0 {
		t.Fatalf("app.css leaves %d blocks open", depth)
	}
}
