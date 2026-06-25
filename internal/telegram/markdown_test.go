package telegram

import "testing"

func TestToTelegramHTML(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"a <b> & c", "a &lt;b&gt; &amp; c"},
		{"use `ls -la` now", "use <code>ls -la</code> now"},
		{"```\ncode <x>\n```", "<pre>code &lt;x&gt;</pre>"},
		{"```go\nfmt.Println()\n```", "<pre>fmt.Println()</pre>"},
		{"before ```\nx\n``` after", "before <pre>x</pre> after"},
		// Unclosed fence (split mid-block) still yields balanced HTML.
		{"```\nunclosed", "<pre>unclosed</pre>"},
		// Unmatched inline backtick kept literal.
		{"a `b", "a `b"},
		// Prose marks: bold, strikethrough, link, heading→bold.
		{"make **this** bold", "make <b>this</b> bold"},
		{"~~gone~~", "<s>gone</s>"},
		{"see [docs](https://x.io/a)", `see <a href="https://x.io/a">docs</a>`},
		{"## Section", "<b>## Section</b>"},
		{"### Deep header ###", "<b>### Deep header</b>"},
		// Heading containing bold must NOT nest <b> (Telegram 400s on that).
		{"## see **this**", "## see <b>this</b>"},
		// Italic is deliberately NOT converted (snake_case false positives).
		{"call do_the_thing()", "call do_the_thing()"},
		{"a * b * c", "a * b * c"},
		// Bold is not applied inside a code span.
		{"`**x**`", "<code>**x**</code>"},
		// A bold link converts both.
		{"[**b**](u)", `<a href="u"><b>b</b></a>`},
	}
	for _, c := range cases {
		if got := toTelegramHTML(c.in); got != c.want {
			t.Errorf("toTelegramHTML(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
