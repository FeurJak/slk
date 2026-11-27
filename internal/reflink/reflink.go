// Package reflink turns bare issue and pull request references in
// message text into Slack links: owner/repo#N and repo#N to GitHub, and
// TEAM-N to Linear for configured team keys. It never links inside code,
// an existing <…> link or mention, a [label](url) link, or a bare URL.
// Pure text in, text out; the org, workspace and teams come from the
// caller (slk's [agent] config), never from this package.
package reflink

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gammons/slk/internal/linkscheme"
)

// Rules says where references point. A zero Rules still links
// owner/repo#N, which names its own owner.
type Rules struct {
	// GitHubOrg is the owner a bare repo#N belongs to.
	GitHubOrg string
	// LinearWorkspace is the linear.app/<workspace> path segment.
	LinearWorkspace string
	// LinearTeams are the team keys (EXP, FRO, …) whose TEAM-N ids link.
	// Matched case-sensitively after upper-casing.
	LinearTeams []string
}

// Link is one reference Linkify turned into a link: the text as written
// and where it now points.
type Link struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

var (
	gitHubRefRe = regexp.MustCompile(`(?:([A-Za-z0-9][A-Za-z0-9-]*)/)?([A-Za-z0-9._-]+)#([0-9]+)`)
	linearRefRe = regexp.MustCompile(`([A-Z][A-Z0-9]*)-([0-9]+)`)

	ownerRe   = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)
	segmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	teamRe    = regexp.MustCompile(`^[A-Z][A-Z0-9]*$`)

	bareURLRe  = regexp.MustCompile(`(?:` + linkscheme.Alternation + `|www\.)[^\s<>]*`)
	mdLinkRe   = regexp.MustCompile(`^\[[^\]\n]*\]\([^)\s]*\)`)
	codeFence  = "```"
	maxRefNums = 12 // longer digit runs are not issue numbers
)

// candidate is one reference found in the text, as the byte range it
// spans and the link it becomes.
type candidate struct {
	start, end int
	url        string
}

// Linkify returns text with every bare reference replaced by
// <url|reference>, and the links it made in text order (nil for none).
func Linkify(text string, r Rules) (string, []Link) {
	protected := protectedSpans(text)
	var cands []candidate
	for _, m := range gitHubRefRe.FindAllStringSubmatchIndex(text, -1) {
		owner, repo, num := group(text, m, 1), group(text, m, 2), group(text, m, 3)
		if owner == "" {
			owner = r.GitHubOrg
		}
		if !ownerRe.MatchString(owner) || len(repo) < 2 || repo == "." || repo == ".." || len(num) > maxRefNums {
			continue
		}
		cands = append(cands, candidate{m[0], m[1], "https://github.com/" + owner + "/" + repo + "/issues/" + num})
	}
	if segmentRe.MatchString(r.LinearWorkspace) {
		teams := teamSet(r.LinearTeams)
		for _, m := range linearRefRe.FindAllStringSubmatchIndex(text, -1) {
			if !teams[group(text, m, 1)] || len(group(text, m, 2)) > maxRefNums {
				continue
			}
			cands = append(cands, candidate{m[0], m[1], "https://linear.app/" + r.LinearWorkspace + "/issue/" + text[m[0]:m[1]]})
		}
	}
	kept := cands[:0]
	for _, c := range cands {
		if standsAlone(text, c.start, c.end) && !overlapsAny(c.start, c.end, protected) {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		return text, nil
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].start < kept[j].start })

	var b strings.Builder
	var links []Link
	last := 0
	for _, c := range kept {
		if c.start < last {
			continue // overlaps the previous reference; the first wins
		}
		ref := text[c.start:c.end]
		b.WriteString(text[last:c.start])
		b.WriteString("<" + c.url + "|" + ref + ">")
		links = append(links, Link{Text: ref, URL: c.url})
		last = c.end
	}
	b.WriteString(text[last:])
	return b.String(), links
}

// group is submatch i of m in text, "" when it did not take part.
func group(text string, m []int, i int) string {
	if m[2*i] < 0 {
		return ""
	}
	return text[m[2*i]:m[2*i+1]]
}

// teamSet is the valid team keys of teams, upper-cased.
func teamSet(teams []string) map[string]bool {
	set := map[string]bool{}
	for _, t := range teams {
		if t = strings.ToUpper(strings.TrimSpace(t)); teamRe.MatchString(t) {
			set[t] = true
		}
	}
	return set
}

// standsAlone reports whether text[start:end] is a whole token: not
// continued by a word character, and not preceded by one or by a
// character that makes it part of a path, address, channel, version or
// another reference.
func standsAlone(text string, start, end int) bool {
	if start > 0 {
		prev, _ := utf8.DecodeLastRuneInString(text[:start])
		if isWordRune(prev) || strings.ContainsRune("./#@-", prev) {
			return false
		}
	}
	if end < len(text) {
		next, _ := utf8.DecodeRuneInString(text[end:])
		if isWordRune(next) {
			return false
		}
	}
	return true
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func overlapsAny(start, end int, spans [][2]int) bool {
	for _, s := range spans {
		if start < s[1] && s[0] < end {
			return true
		}
	}
	return false
}

// protectedSpans are the byte ranges no reference may be linked in:
// closed code fences and inline code, <…> (Slack links, mentions,
// dates), [label](url) links, and bare URLs. An unclosed backtick or
// bracket is literal text, as Slack treats it.
func protectedSpans(text string) [][2]int {
	var spans [][2]int
	for i := 0; i < len(text); {
		if strings.HasPrefix(text[i:], codeFence) {
			if j := strings.Index(text[i+len(codeFence):], codeFence); j >= 0 {
				end := i + len(codeFence) + j + len(codeFence)
				spans = append(spans, [2]int{i, end})
				i = end
				continue
			}
		}
		switch text[i] {
		case '`':
			if j := strings.IndexByte(text[i+1:], '`'); j >= 0 {
				end := i + 1 + j + 1
				spans = append(spans, [2]int{i, end})
				i = end
				continue
			}
		case '<':
			if j := strings.IndexByte(text[i+1:], '>'); j >= 0 {
				end := i + 1 + j + 1
				spans = append(spans, [2]int{i, end})
				i = end
				continue
			}
		case '[':
			if loc := mdLinkRe.FindStringIndex(text[i:]); loc != nil {
				spans = append(spans, [2]int{i, i + loc[1]})
				i += loc[1]
				continue
			}
		}
		i++
	}
	for _, loc := range bareURLRe.FindAllStringIndex(text, -1) {
		spans = append(spans, [2]int{loc[0], loc[1]})
	}
	return spans
}
