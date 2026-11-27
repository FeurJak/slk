package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gammons/slk/internal/config"
)

// writeOut decodes a write result; blocks stay raw, as slack.Block is
// an interface encoding/json cannot fill.
type writeOut struct {
	agentWriteResult
	Blocks []json.RawMessage `json:"blocks"`
}

// writeRoutes is a fake Slack for the write commands: bob is findable,
// #general is joined, and a self DM exists.
func writeRoutes(extra map[string]func(agentReq) string) map[string]func(agentReq) string {
	routes := map[string]func(agentReq) string{
		"/api/users.conversations": fixed(agentConversations),
		"/cache/T1/users/info":     agentUsersRoute(),
		"/cache/T1/users/search": func(r agentReq) string {
			if strings.Contains(r.body, `"query":"bob"`) {
				return `{"ok":true,"results":[` + userBob + `]}`
			}
			return `{"ok":true,"results":[]}`
		},
		"/api/usergroups.list": fixed(`{"ok":true,"usergroups":[{"id":"S1","handle":"devs","name":"Devs"}]}`),
	}
	for k, v := range extra {
		routes[k] = v
	}
	return routes
}

func TestAgentSend_DryRunTranslatesAndSendsNothing(t *testing.T) {
	f := newAgentFake(t, writeRoutes(nil))
	var res writeOut
	runJSON(t, f.session(t), "", &res, "send", "#general", "hi @bob and @devs, see #general @nobody", "--dry-run")
	if len(f.requests("/api/chat.postMessage")) != 0 {
		t.Fatal("dry run posted a message")
	}
	if !res.DryRun || res.ChannelID != "C1" || res.Method != "chat.postMessage" || len(res.Blocks) != 1 {
		t.Errorf("result = %+v", res)
	}
	for _, want := range []string{"<@U2>", "<!subteam^S1>", "<#C1>"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("text %q lacks %s", res.Text, want)
		}
	}
	if strings.Join(res.UnresolvedMentions, ",") != "nobody" {
		t.Errorf("unresolved = %v, want [nobody]", res.UnresolvedMentions)
	}
}

func TestAgentSend_PostsToTheSelfDMAndLinksIt(t *testing.T) {
	f := newAgentFake(t, writeRoutes(map[string]func(agentReq) string{
		"/api/chat.postMessage": fixed(`{"ok":true,"channel":"D2","ts":"1795700000.000100"}`),
	}))
	var res writeOut
	runJSON(t, f.session(t), "*bold* note\n", &res, "send", "@me", "-")
	posts := f.requests("/api/chat.postMessage")
	if len(posts) != 1 {
		t.Fatalf("posts = %d", len(posts))
	}
	p := posts[0].form
	if p.Get("channel") != "D2" || p.Get("text") != "*bold* note" || !strings.Contains(p.Get("blocks"), "rich_text") {
		t.Errorf("chat.postMessage form = %v", p)
	}
	if res.TS != "1795700000.000100" || res.Permalink != "https://acme.slack.com/archives/D2/p1795700000000100" {
		t.Errorf("result = %+v", res)
	}
}

func TestAgentSend_UsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"send", "C1"},
		{"send", "C1", "hi", "there"},
		{"send", "C1", "hi", "--broadcast"},
		{"send", "C1", "hi", "--thread", "soon"},
		{"send", "C1", "   "},
	} {
		if _, _, code := runAgentForTest(t, nil, "", args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestAgentReply_ThreadsUnderTheParent(t *testing.T) {
	f := newAgentFake(t, writeRoutes(map[string]func(agentReq) string{
		"/api/chat.postMessage": fixed(`{"ok":true,"channel":"C1","ts":"1700000400.000000"}`),
	}))
	var res writeOut
	runJSON(t, f.session(t), "", &res, "reply", "https://acme.slack.com/archives/C1/p1700000301000000?thread_ts=1700000300.000000&cid=C1", "ok", "--broadcast")
	p := f.requests("/api/chat.postMessage")[0].form
	if p.Get("thread_ts") != "1700000300.000000" || p.Get("reply_broadcast") != "true" {
		t.Errorf("form = %v; want the parent's thread, broadcast", p)
	}
	if res.ThreadTS != "1700000300.000000" || !strings.Contains(res.Permalink, "thread_ts=1700000300.000000") {
		t.Errorf("result = %+v", res)
	}
}

func TestAgentReact_StripsColonsAndAcceptsAlreadyReacted(t *testing.T) {
	f := newAgentFake(t, writeRoutes(map[string]func(agentReq) string{
		"/api/reactions.add": fixed(`{"ok":false,"error":"already_reacted"}`),
	}))
	var res writeOut
	runJSON(t, f.session(t), "", &res, "react", "https://acme.slack.com/archives/C1/p1700000300000000", ":tada:")
	add := f.requests("/api/reactions.add")
	if len(add) != 1 || add[0].form.Get("name") != "tada" || add[0].form.Get("timestamp") != "1700000300.000000" {
		t.Fatalf("reactions.add = %+v", add)
	}
	if !res.OK || res.Emoji != "tada" {
		t.Errorf("result = %+v", res)
	}
}

func TestAgentDelete_RefusesSomeoneElsesMessage(t *testing.T) {
	f := newAgentFake(t, writeRoutes(map[string]func(agentReq) string{
		"/api/conversations.replies": fixed(`{"ok":true,"messages":[{"ts":"1700000300.000000","user":"U2","text":"bob's"}]}`),
	}))
	for _, args := range [][]string{
		{"delete", "https://acme.slack.com/archives/C1/p1700000300000000"},
		{"edit", "https://acme.slack.com/archives/C1/p1700000300000000", "mine now"},
	} {
		_, stderr, code := runAgentForTest(t, f.session(t), "", args...)
		if code != 1 || decodeAgentError(t, stderr).Kind != "refused" {
			t.Errorf("%s: exit %d, %s", args[0], code, stderr)
		}
	}
	if len(f.requests("/api/chat.delete"))+len(f.requests("/api/chat.update")) != 0 {
		t.Error("a refused command reached chat.delete or chat.update")
	}
}

func TestAgentDelete_OwnMessage(t *testing.T) {
	f := newAgentFake(t, writeRoutes(map[string]func(agentReq) string{
		"/api/conversations.replies": fixed(`{"ok":true,"messages":[{"ts":"1700000300.000000","user":"U1","text":"mine"}]}`),
		"/api/chat.delete":           fixed(`{"ok":true,"channel":"D2","ts":"1700000300.000000"}`),
	}))
	link := "https://acme.slack.com/archives/D2/p1700000300000000"
	var res writeOut
	runJSON(t, f.session(t), "", &res, "delete", link, "--dry-run")
	if !res.DryRun || len(f.requests("/api/chat.delete")) != 0 {
		t.Fatalf("dry run: %+v, %d deletes", res, len(f.requests("/api/chat.delete")))
	}
	runJSON(t, f.session(t), "", &res, "delete", link)
	del := f.requests("/api/chat.delete")
	if len(del) != 1 || del[0].form.Get("channel") != "D2" || del[0].form.Get("ts") != "1700000300.000000" {
		t.Errorf("chat.delete = %+v", del)
	}
	if r := f.requests("/api/conversations.replies")[0].form; r.Get("oldest") != "1700000300.000000" || r.Get("latest") != "1700000300.000000" {
		t.Errorf("own-message check asked for %v; want exactly that ts", r)
	}
}

func TestAgentEdit_OwnMessageTranslatesMentions(t *testing.T) {
	f := newAgentFake(t, writeRoutes(map[string]func(agentReq) string{
		"/api/conversations.replies": fixed(`{"ok":true,"messages":[{"ts":"1700000301.000000","user":"U1","text":"old","thread_ts":"1700000300.000000"}]}`),
		"/api/chat.update":           fixed(`{"ok":true,"channel":"D2","ts":"1700000301.000000","text":"x"}`),
	}))
	var res writeOut
	runJSON(t, f.session(t), "", &res, "edit", "https://acme.slack.com/archives/D2/p1700000301000000?thread_ts=1700000300.000000&cid=D2", "now with @bob")
	up := f.requests("/api/chat.update")
	if len(up) != 1 || up[0].form.Get("ts") != "1700000301.000000" || !strings.Contains(up[0].form.Get("text"), "<@U2>") {
		t.Fatalf("chat.update = %+v", up)
	}
	if r := f.requests("/api/conversations.replies")[0].form; r.Get("ts") != "1700000300.000000" {
		t.Errorf("own-message check read thread %q, want the reply's thread", r.Get("ts"))
	}
	if res.Action != "edit" || res.ThreadTS != "1700000300.000000" {
		t.Errorf("result = %+v", res)
	}
}

func TestAtWords(t *testing.T) {
	got := atWords("hi @bob, mail a@b.com, <@U1>, @here @Bob. @taras.w! url/@x")
	if strings.Join(got, "|") != "bob|taras.w" {
		t.Errorf("atWords = %q, want bob and taras.w once each", got)
	}
}

// linkifySession is a write session whose [agent] config links acme
// repos and the EXP Linear team.
func linkifySession(t *testing.T, f *agentFake) *agentSession {
	t.Helper()
	s := f.session(t)
	s.cfg.Agent = config.Agent{GitHubOrg: "acme", LinearWorkspace: "acmeco", LinearTeams: []string{"EXP"}}
	return s
}

func TestAgentSend_LinkifiesReferences(t *testing.T) {
	f := newAgentFake(t, writeRoutes(nil))
	var res writeOut
	runJSON(t, linkifySession(t, f), "", &res, "send", "#general",
		"fixed frontend#9673 and EXP-61, not `EXP-2` or https://linear.app/acmeco/issue/EXP-3", "--dry-run")
	for _, want := range []string{
		"<https://github.com/acme/frontend/issues/9673|frontend#9673>",
		"<https://linear.app/acmeco/issue/EXP-61|EXP-61>",
		"`EXP-2`",
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("text %q lacks %s", res.Text, want)
		}
	}
	if strings.Contains(res.Text, "issue/EXP-2") || strings.Count(res.Text, "issue/EXP-3") != 1 {
		t.Errorf("text %q linked inside code or a URL", res.Text)
	}
	if len(res.Links) != 2 || res.Links[0].Text != "frontend#9673" || res.Links[1].URL != "https://linear.app/acmeco/issue/EXP-61" {
		t.Errorf("links = %+v", res.Links)
	}
}

func TestAgentSend_NoLinkify(t *testing.T) {
	f := newAgentFake(t, writeRoutes(nil))
	var res writeOut
	runJSON(t, linkifySession(t, f), "", &res, "send", "#general", "fixed frontend#9673 and EXP-61", "--dry-run", "--no-linkify")
	if res.Text != "fixed frontend#9673 and EXP-61" || len(res.Links) != 0 {
		t.Errorf("text %q, links %+v; want both untouched", res.Text, res.Links)
	}
}

func TestAgentReply_PostsLinkifiedText(t *testing.T) {
	f := newAgentFake(t, writeRoutes(map[string]func(agentReq) string{
		"/api/chat.postMessage": fixed(`{"ok":true,"channel":"C1","ts":"1700000400.000000"}`),
	}))
	var res writeOut
	runJSON(t, linkifySession(t, f), "", &res, "reply", "https://acme.slack.com/archives/C1/p1700000300000000", "merged in gammons/slk#2, thanks @bob")
	p := f.requests("/api/chat.postMessage")[0].form
	if !strings.Contains(p.Get("text"), "<https://github.com/gammons/slk/issues/2|gammons/slk#2>") || !strings.Contains(p.Get("text"), "<@U2>") {
		t.Errorf("posted text = %q", p.Get("text"))
	}
	if !strings.Contains(p.Get("blocks"), `"url":"https://github.com/gammons/slk/issues/2"`) {
		t.Errorf("blocks = %s; want a link element", p.Get("blocks"))
	}
}

func TestAgentEdit_Linkifies(t *testing.T) {
	f := newAgentFake(t, writeRoutes(map[string]func(agentReq) string{
		"/api/conversations.replies": fixed(`{"ok":true,"messages":[{"ts":"1700000300.000000","user":"U1","text":"old"}]}`),
	}))
	var res writeOut
	runJSON(t, linkifySession(t, f), "", &res, "edit", "https://acme.slack.com/archives/D2/p1700000300000000", "see EXP-7", "--dry-run")
	if !strings.Contains(res.Text, "<https://linear.app/acmeco/issue/EXP-7|EXP-7>") || len(res.Links) != 1 {
		t.Errorf("text %q, links %+v", res.Text, res.Links)
	}
}
