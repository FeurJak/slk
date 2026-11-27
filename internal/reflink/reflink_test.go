package reflink

import (
	"strings"
	"testing"
)

var rules = Rules{GitHubOrg: "acme", LinearWorkspace: "acmeco", LinearTeams: []string{"EXP", "fro"}}

func TestLinkify(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"bare repo uses the org", "fixed in frontend#9673.", "fixed in <https://github.com/acme/frontend/issues/9673|frontend#9673>."},
		{"owner/repo", "see gammons/slk#42", "see <https://github.com/gammons/slk/issues/42|gammons/slk#42>"},
		{"repo with dots and dashes", "(my.repo-x#7)", "(<https://github.com/acme/my.repo-x/issues/7|my.repo-x#7>)"},
		{"linear team", "EXP-61 and FRO-286!", "<https://linear.app/acmeco/issue/EXP-61|EXP-61> and <https://linear.app/acmeco/issue/FRO-286|FRO-286>!"},
		{"start of line and newline", "EXP-1\napi#2", "<https://linear.app/acmeco/issue/EXP-1|EXP-1>\n<https://github.com/acme/api/issues/2|api#2>"},
		{"unknown team", "UTF-8 and SHA-256 and QA-82", "UTF-8 and SHA-256 and QA-82"},
		{"lowercase team text", "exp-61", "exp-61"},
		{"inline code", "run `frontend#1` and `EXP-2`", "run `frontend#1` and `EXP-2`"},
		{"code block", "```\nfrontend#1\nEXP-2\n```\napi#3", "```\nfrontend#1\nEXP-2\n```\n<https://github.com/acme/api/issues/3|api#3>"},
		{"slack link", "<https://x.test/frontend#1|frontend#1> EXP-2 <@U1> <#C1|frontend>", "<https://x.test/frontend#1|frontend#1> <https://linear.app/acmeco/issue/EXP-2|EXP-2> <@U1> <#C1|frontend>"},
		{"markdown link", "[EXP-61](https://linear.app/acmeco/issue/EXP-61) done", "[EXP-61](https://linear.app/acmeco/issue/EXP-61) done"},
		{"bare url", "https://linear.app/acmeco/issue/EXP-61 and https://github.com/acme/frontend/pull/9673#x", "https://linear.app/acmeco/issue/EXP-61 and https://github.com/acme/frontend/pull/9673#x"},
		{"schemeless github path", "github.com/acme/frontend#12", "github.com/acme/frontend#12"},
		{"glued to words", "xEXP-1 EXP-1y EXP-1_ a@frontend#1 a/b/frontend#1 v1.EXP-2 #frontend#3 frontend#12a", "xEXP-1 EXP-1y EXP-1_ a@frontend#1 a/b/frontend#1 v1.EXP-2 #frontend#3 frontend#12a"},
		{"dotted words are repos", "v1.frontend#2", "<https://github.com/acme/v1.frontend/issues/2|v1.frontend#2>"},
		{"channel and bare number", "#general and #42", "#general and #42"},
		{"one-letter repo", "C#10", "C#10"},
		{"unclosed backtick is literal", "it's `odd EXP-5", "it's `odd <https://linear.app/acmeco/issue/EXP-5|EXP-5>"},
		{"bold", "*frontend#5*", "*<https://github.com/acme/frontend/issues/5|frontend#5>*"},
	}
	for _, c := range cases {
		got, _ := Linkify(c.in, rules)
		if got != c.want {
			t.Errorf("%s:\n in   %q\n got  %q\n want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestLinkify_ReportsLinksInOrder(t *testing.T) {
	_, links := Linkify("EXP-1 then acme/api#2 then EXP-1", rules)
	var got []string
	for _, l := range links {
		got = append(got, l.Text+"="+l.URL)
	}
	want := "EXP-1=https://linear.app/acmeco/issue/EXP-1|acme/api#2=https://github.com/acme/api/issues/2|EXP-1=https://linear.app/acmeco/issue/EXP-1"
	if strings.Join(got, "|") != want {
		t.Errorf("links = %q", got)
	}
}

func TestLinkify_WithoutConfig(t *testing.T) {
	in := "frontend#1 owner/repo#2 EXP-3"
	got, links := Linkify(in, Rules{})
	if want := "frontend#1 <https://github.com/owner/repo/issues/2|owner/repo#2> EXP-3"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if len(links) != 1 {
		t.Errorf("links = %v", links)
	}
}

func TestLinkify_IgnoresInvalidConfig(t *testing.T) {
	bad := Rules{GitHubOrg: "ac me/../x", LinearWorkspace: "a/b", LinearTeams: []string{"E X", "9AB", ""}}
	in := "frontend#1 EXP-2 9AB-3"
	if got, links := Linkify(in, bad); got != in || len(links) != 0 {
		t.Errorf("got %q, %v; want the text unchanged", got, links)
	}
}

func TestLinkify_LinearNeedsAWorkspace(t *testing.T) {
	in := "EXP-2"
	if got, _ := Linkify(in, Rules{LinearTeams: []string{"EXP"}}); got != in {
		t.Errorf("got %q; a team without a workspace must not link", got)
	}
}

func TestLinkify_NoReferencesIsIdentity(t *testing.T) {
	for _, in := range []string{"", "plain text", "<@U1> hi", "```\ncode\n```"} {
		if got, links := Linkify(in, rules); got != in || links != nil {
			t.Errorf("Linkify(%q) = %q, %v", in, got, links)
		}
	}
}
