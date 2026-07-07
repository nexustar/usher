package main

import "testing"

func TestSanitizeMarkdown(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"tag escaped", "hi <at id=all></at>", "hi &#60;at id=all>&#60;/at>"},
		{"formatting passes through", "**bold** and *em*\n- item\n# head", "**bold** and *em*\n- item\n# head"},
		{"inline code preserved", "use `<at id=all>` here <b>", "use `<at id=all>` here &#60;b>"},
		{"double-backtick span", "a ``code with ` tick and <x>`` b <y>", "a ``code with ` tick and <x>`` b &#60;y>"},
		{"unmatched backtick escapes rest", "odd ` tick <x>", "odd ` tick &#60;x>"},
		{"fenced block preserved", "before <a>\n```go\nif x < y { <at id=all> }\n```\nafter <b>",
			"before &#60;a>\n```go\nif x < y { <at id=all> }\n```\nafter &#60;b>"},
		{"indented fence recognized", "  ```\n<raw>\n  ```\n<out>", "  ```\n<raw>\n  ```\n&#60;out>"},
		{"unclosed fence stays literal", "```\n<inside>", "```\n<inside>"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := sanitizeMarkdown(c.in); got != c.want {
			t.Errorf("%s:\n in  %q\n got %q\n want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestQuoteMD(t *testing.T) {
	if got := quoteMD("a\nb\n\nc"); got != "> a\n> b\n> \n> c" {
		t.Fatalf("quoteMD = %q", got)
	}
}
