package syncer

import "testing"

func TestExcludeMatcherPaths(t *testing.T) {
	m := newExcludeMatcher(nil, []string{"Sub/Deep", "a.txt", "/trailing/"})

	cases := []struct {
		rel  string
		want bool
	}{
		{"sub/deep", true},          // exact path
		{"SUB/DEEP/file.bin", true}, // descendant, case-insensitive
		{"sub/deepx/file.bin", false},
		{"sub", false},
		{"a.txt", true},
		{"b/a.txt", false},
		{"trailing/x", true},
		{"other/file.bin", false},
	}
	for _, c := range cases {
		if got := m.matchPath(c.rel); got != c.want {
			t.Errorf("matchPath(%q)=%v, want %v", c.rel, got, c.want)
		}
	}
}
