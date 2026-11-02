package interview

import "testing"

func TestChangelog(t *testing.T) {
	md := "# PRD\n\n## 13. Risks\n- r\n\n## Changelog\n- v2: added export (asked by asmith)\n- not changed: pricing, out of scope\n\n## Something after\nignored\n"
	got := Changelog(md)
	want := "- v2: added export (asked by asmith)\n- not changed: pricing, out of scope"
	if got != want {
		t.Fatalf("got %q", got)
	}
	if Changelog("# PRD\n## Changelog\n") != "" || Changelog("# PRD only") != "" {
		t.Fatal("blank or missing changelog must be empty")
	}
	if got := Changelog("## changelog\nlast section, no trailing heading"); got != "last section, no trailing heading" {
		t.Fatalf("got %q", got)
	}
}
