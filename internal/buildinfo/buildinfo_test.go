package buildinfo

import "testing"

func TestWithBuildAddsTheFirstTwelveCharactersOfTheDigest(t *testing.T) {
	saved := Version
	t.Cleanup(func() { Version = saved })
	Version = "1.2.3"
	for _, test := range []struct{ build, want string }{
		{"", "1.2.3"},
		{"abcdef01234", "1.2.3"},
		{"abcdef012345", "1.2.3+abcdef012345"},
		{"abcdef0123456789", "1.2.3+abcdef012345"},
	} {
		if got := WithBuild(test.build); got != test.want {
			t.Errorf("WithBuild(%q) = %q, want %q", test.build, got, test.want)
		}
	}
}
