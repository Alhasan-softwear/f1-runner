package sshx

import "testing"

func TestQuote(t *testing.T) {
	cases := map[string]string{
		"plain":        "plain",
		"/opt/f1/bin":  "/opt/f1/bin",
		"a b":          "'a b'",
		"it's":         `'it'\''s'`,
		"":             "''",
		"a;rm -rf /":   "'a;rm -rf /'",
		"$HOME":        "'$HOME'",
		"back`tick`":   "'back`tick`'",
		"comp1,comp2":  "comp1,comp2",
		"user@host:22": "user@host:22",
	}
	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestQuoteCmd(t *testing.T) {
	got := QuoteCmd([]string{"/opt/f1/bin/f1", "apply", "--ref", "my branch"})
	want := "/opt/f1/bin/f1 apply --ref 'my branch'"
	if got != want {
		t.Errorf("QuoteCmd = %s, want %s", got, want)
	}
}
