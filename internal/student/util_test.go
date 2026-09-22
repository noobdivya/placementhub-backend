package student

import (
	"strings"
	"testing"
)

func TestSafeFilename(t *testing.T) {
	cases := map[string]string{
		"Ananya_Sharma_Resume.pdf": "Ananya_Sharma_Resume.pdf",
		"resume":                   "resume.pdf",
		"../../etc/passwd":         "passwd.pdf",
		`..\..\windows\system32`:   "system32.pdf",
		`my"file;name.docx`:        "myfilename.pdf",
		"":                         "resume.pdf",
		"..":                       "resume.pdf",
		"a\r\nb\x00c.pdf":          "abc.pdf",
	}
	for in, want := range cases {
		if got := SafeFilename(in); got != want {
			t.Errorf("SafeFilename(%q) = %q, want %q", in, got, want)
		}
	}
	long := SafeFilename(strings.Repeat("a", 500) + ".pdf")
	if len([]rune(long)) > 84 {
		t.Errorf("filename not capped: %d runes", len([]rune(long)))
	}
}

func TestNormalizeSkill(t *testing.T) {
	ok := map[string]string{"  React  ": "React", "System   Design": "System Design", "C/C++": "C/C++", "Node.js": "Node.js"}
	for in, want := range ok {
		if got, valid := normalizeSkill(in); !valid || got != want {
			t.Errorf("normalizeSkill(%q) = %q, %v", in, got, valid)
		}
	}
	for _, in := range []string{"", "   ", strings.Repeat("x", 41), "bad\x00skill", "bell\x07"} {
		if _, valid := normalizeSkill(in); valid {
			t.Errorf("normalizeSkill(%q) accepted", in)
		}
	}
}

func TestEscapeLike(t *testing.T) {
	if got := escapeLike(`50%_\done`); got != `50\%\_\\done` {
		t.Errorf("escapeLike = %q", got)
	}
}
