package update

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	semver = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)
	// describe is what make build stamps (git describe --tags --dirty): commits past a tag, a dirty tree, or both.
	describe = regexp.MustCompile(`^(v\d+\.\d+\.\d+)(?:-\d+-g[0-9a-f]+)?(?:-dirty)?$`)
)

type version struct {
	n     [3]int
	pre   []string
	after bool // a build past its tag, which git describe names by the tag
}

// parse reads a release tag, a Go pseudo-version or a git describe name; a build without one ("dev") is
// not a version.
func parse(s string) (version, bool) {
	var v version
	if m := describe.FindStringSubmatch(s); m != nil && m[1] != s {
		s, v.after = m[1], true
	}
	m := semver.FindStringSubmatch(s)
	if m == nil {
		return v, false
	}
	for i := range v.n {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return v, false
		}
		v.n[i] = n
	}
	if m[4] != "" {
		v.pre = strings.Split(m[4], ".")
	}
	return v, true
}

// Known reports whether v names a version that releases can be compared with.
func Known(v string) bool {
	_, ok := parse(v)
	return ok
}

// Newer reports whether release is later than current; never when either is not a version.
func Newer(release, current string) bool {
	r, ok := parse(release)
	c, ok2 := parse(current)
	return ok && ok2 && compare(r, c) > 0
}

func compare(a, b version) int {
	if c := slices.Compare(a.n[:], b.n[:]); c != 0 {
		return c
	}
	if a.after != b.after {
		if a.after {
			return 1
		}
		return -1
	}
	return comparePre(a.pre, b.pre)
}

// comparePre orders prereleases as semver does; a release, with none, comes after all of its own.
func comparePre(a, b []string) int {
	if len(a) == 0 || len(b) == 0 {
		return cmp.Compare(len(b), len(a))
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		an, aerr := strconv.Atoi(a[i])
		bn, berr := strconv.Atoi(b[i])
		var c int
		switch {
		case aerr == nil && berr == nil:
			c = cmp.Compare(an, bn)
		case aerr == nil:
			c = -1
		case berr == nil:
			c = 1
		default:
			c = strings.Compare(a[i], b[i])
		}
		if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a), len(b))
}
