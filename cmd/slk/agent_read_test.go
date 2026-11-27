package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gammons/slk/internal/config"
	slackclient "github.com/gammons/slk/internal/slack"
)

// agentReq is one request the agent fake received.
type agentReq struct {
	path string
	form url.Values
	body string
}

// agentFake is a TLS fake Slack whose answers are functions of the
// request, recording every request. Unrouted paths fail the test.
type agentFake struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []agentReq
}

const agentAuthTest = `{"ok":true,"url":"https://acme.slack.com/","team":"Acme","user":"me","team_id":"T1","user_id":"U1"}`

func newAgentFake(t *testing.T, routes map[string]func(agentReq) string) *agentFake {
	t.Helper()
	f := &agentFake{}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := agentReq{path: r.URL.Path, body: string(raw)}
		req.form, _ = url.ParseQuery(string(raw))
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		f.mu.Unlock()
		route, ok := routes[r.URL.Path]
		if r.URL.Path == "/api/auth.test" && !ok {
			route, ok = func(agentReq) string { return agentAuthTest }, true
		}
		if !ok {
			t.Errorf("agent fake: unexpected request to %s", r.URL.Path)
			http.Error(w, `{"ok":false,"error":"unexpected_path"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, route(req))
	}))
	t.Cleanup(f.Close)
	return f
}

// session connects a client to the fake and wraps it.
func (f *agentFake) session(t *testing.T) *agentSession {
	t.Helper()
	c := newTestClient(t, f.Server)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return newAgentSession(c, "xoxc-test", config.Config{}, false)
}

func (f *agentFake) requests(path string) []agentReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []agentReq
	for _, r := range f.reqs {
		if r.path == path {
			out = append(out, r)
		}
	}
	return out
}

// fixed answers body to every request.
func fixed(body string) func(agentReq) string {
	return func(agentReq) string { return body }
}

// edgeUsers answers edge users/info with the users asked for, out of
// known (id -> result JSON object).
func edgeUsers(known map[string]string) func(agentReq) string {
	return func(r agentReq) string {
		var body struct {
			UpdatedIDs map[string]int64 `json:"updated_ids"`
		}
		_ = json.Unmarshal([]byte(r.body), &body)
		var results []string
		for id := range body.UpdatedIDs {
			if u, ok := known[id]; ok {
				results = append(results, u)
			}
		}
		slices.Sort(results)
		return `{"ok":true,"results":[` + strings.Join(results, ",") + `]}`
	}
}

const (
	userMe  = `{"id":"U1","name":"me","team_id":"T1","tz":"Australia/Canberra","profile":{"display_name":"Me Myself","real_name":"Me Real","title":"Lead"}}`
	userBob = `{"id":"U2","name":"bob","team_id":"T1","tz":"Europe/Warsaw","profile":{"display_name":"Bobby","real_name":"Bob Builder","title":""}}`
)

func agentUsersRoute() func(agentReq) string {
	return edgeUsers(map[string]string{"U1": userMe, "U2": userBob})
}

// runJSON runs a command that must succeed and decodes its output.
func runJSON(t *testing.T, s *agentSession, stdin string, out any, args ...string) {
	t.Helper()
	stdout, stderr, code := runAgentForTest(t, s, stdin, args...)
	if code != 0 {
		t.Fatalf("%v: exit %d, stderr %s", args, code, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), out); err != nil {
		t.Fatalf("%v: stdout is not JSON: %v\n%s", args, err, stdout)
	}
}

func TestAgentSearch_ResolvesNamesAndThreads(t *testing.T) {
	f := newAgentFake(t, map[string]func(agentReq) string{
		"/api/search.messages": fixed(`{"ok":true,"query":"q","messages":{"total":2,
			"paging":{"count":20,"total":2,"page":2,"pages":3},
			"matches":[
			 {"channel":{"id":"C9","name":"frontend"},"user":"U2","username":"bob","ts":"1795634655.404329",
			  "text":"ping <@U1> about Linear","permalink":"https://acme.slack.com/archives/C9/p1795634655404329?thread_ts=1795634600.000100&cid=C9"},
			 {"channel":{"id":"D5","name":"U2"},"user":"U1","username":"me","ts":"1795634000.000001",
			  "text":"dm","permalink":"https://acme.slack.com/archives/D5/p1795634000000001"}]}}`),
		"/cache/T1/users/info": agentUsersRoute(),
	})
	var res agentSearchResult
	runJSON(t, f.session(t), "", &res, "search", "in:#frontend", "from:@bob", "Linear", "--page", "2", "--sort", "timestamp")

	req := f.requests("/api/search.messages")[0]
	if got := req.form.Get("query"); got != "in:#frontend from:@bob Linear" {
		t.Errorf("query = %q, want the modifiers verbatim", got)
	}
	if req.form.Get("page") != "2" || req.form.Get("sort") != "timestamp" {
		t.Errorf("page %q sort %q; want 2, timestamp", req.form.Get("page"), req.form.Get("sort"))
	}
	if res.Total != 2 || res.Page != 2 || res.Pages != 3 || len(res.Matches) != 2 {
		t.Fatalf("result = %+v", res)
	}
	hit := res.Matches[0]
	if hit.ThreadTS != "1795634600.000100" || hit.UserName != "Bobby" || hit.UserHandle != "bob" || hit.ChannelName != "frontend" {
		t.Errorf("hit = %+v; want thread from permalink, Bobby/bob, #frontend", hit)
	}
	if hit.TextPlain != "ping @Me Myself about Linear" || hit.Text != "ping <@U1> about Linear" {
		t.Errorf("text %q plain %q", hit.Text, hit.TextPlain)
	}
	if dm := res.Matches[1]; dm.ChannelType != "im" || dm.ChannelName != "Bobby" {
		t.Errorf("DM hit channel = %q (%s); want Bobby (im)", dm.ChannelName, dm.ChannelType)
	}
	if n := len(f.requests("/cache/T1/users/info")); n != 1 {
		t.Errorf("users/info requests = %d, want one batch", n)
	}
}

func TestAgentSearch_RejectsBadFlagsBeforeConnecting(t *testing.T) {
	for _, args := range [][]string{
		{"search"},
		{"search", "q", "--count", "0"},
		{"search", "q", "--sort", "newest"},
		{"search", "q", "--sort-dir", "up"},
	} {
		if _, _, code := runAgentForTest(t, nil, "", args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

const agentConversations = `{"ok":true,"channels":[
	{"id":"C1","name":"general","is_channel":true,"topic":{"value":"talk"}},
	{"id":"G1","name":"secret","is_group":true,"is_private":true},
	{"id":"G2","name":"mpdm-me--bob-1","is_mpim":true,"is_private":true,"members":["U1","U2"]},
	{"id":"D1","is_im":true,"user":"U2"},
	{"id":"D2","is_im":true,"user":"U1"}
],"response_metadata":{"next_cursor":""}}`

func TestAgentChannels_TypesPeersAndSelfDM(t *testing.T) {
	f := newAgentFake(t, map[string]func(agentReq) string{
		"/api/users.conversations": fixed(agentConversations),
		"/cache/T1/users/info":     agentUsersRoute(),
	})
	var res struct {
		Channels []agentChannel `json:"channels"`
	}
	runJSON(t, f.session(t), "", &res, "channels", "--type", "mpim,im")
	if len(res.Channels) != 3 {
		t.Fatalf("channels = %+v, want the group DM and two DMs", res.Channels)
	}
	byID := map[string]agentChannel{}
	for _, ch := range res.Channels {
		byID[ch.ID] = ch
	}
	if d := byID["D1"]; d.UserName != "Bobby" || d.UserHandle != "bob" || d.IsSelf || *d.MemberCount != 2 || d.Name != "Bobby" {
		t.Errorf("DM with bob = %+v", d)
	}
	if d := byID["D2"]; !d.IsSelf || *d.MemberCount != 1 {
		t.Errorf("self DM = %+v, want is_self and one member", d)
	}
	if g := byID["G2"]; g.Type != "mpim" || g.Name != "Bobby" || *g.MemberCount != 2 {
		t.Errorf("group DM = %+v", g)
	}
	if res.Channels[0].Type != "mpim" {
		t.Errorf("order = %+v, want group DMs before DMs", res.Channels)
	}
}

func TestAgentChannels_MemberCountOnlyWhenAsked(t *testing.T) {
	f := newAgentFake(t, map[string]func(agentReq) string{
		"/api/users.conversations": fixed(agentConversations),
		"/cache/T1/users/info":     agentUsersRoute(),
		"/cache/T1/users/counts":   fixed(`{"ok":true,"counts":{"everyone":36,"people":32}}`),
	})
	s := f.session(t)
	var res struct {
		Channels []agentChannel `json:"channels"`
	}
	runJSON(t, s, "", &res, "channels", "--type", "public")
	if len(res.Channels) != 1 || res.Channels[0].MemberCount != nil || len(f.requests("/cache/T1/users/counts")) != 0 {
		t.Fatalf("without --member-count: %+v, %d counts requests", res.Channels, len(f.requests("/cache/T1/users/counts")))
	}
	runJSON(t, s, "", &res, "channels", "--type", "public", "--member-count")
	if c := res.Channels[0]; c.MemberCount == nil || *c.MemberCount != 36 || c.Topic != "talk" {
		t.Errorf("with --member-count: %+v", c)
	}
}

func TestAgentUsers_NeedsAQueryOrIDs(t *testing.T) {
	_, stderr, code := runAgentForTest(t, nil, "", "users")
	if code != 2 || !strings.Contains(decodeAgentError(t, stderr).Message, "does not list every user") {
		t.Errorf("exit %d, %s", code, stderr)
	}
}

func TestAgentUsers_ByIDKeepsOrderAndReportsMissing(t *testing.T) {
	f := newAgentFake(t, map[string]func(agentReq) string{
		"/cache/T1/users/info": agentUsersRoute(),
		"/api/users.info":      fixed(`{"ok":false,"error":"user_not_found"}`),
	})
	var res struct {
		Users    []agentUser `json:"users"`
		NotFound []string    `json:"not_found"`
	}
	runJSON(t, f.session(t), "", &res, "users", "--id", "U2,U9,U1")
	if len(res.Users) != 2 || res.Users[0].ID != "U2" || res.Users[1].ID != "U1" {
		t.Fatalf("users = %+v", res.Users)
	}
	if u := res.Users[1]; u.Title != "Lead" || u.TZ != "Australia/Canberra" || u.RealName != "Me Real" {
		t.Errorf("me = %+v", u)
	}
	if strings.Join(res.NotFound, ",") != "U9" {
		t.Errorf("not_found = %v", res.NotFound)
	}
}

func TestAgentHistory_LimitOrderAndUntilIsExclusive(t *testing.T) {
	f := newAgentFake(t, map[string]func(agentReq) string{
		"/api/users.conversations": fixed(agentConversations),
		"/cache/T1/users/info":     agentUsersRoute(),
		// Newest first, as Slack pages; 400 is --until itself.
		"/api/conversations.history": fixed(`{"ok":true,"has_more":false,"messages":[
			{"ts":"400.000000","user":"U2","text":"at until"},
			{"ts":"300.000000","user":"U2","text":"three","thread_ts":"300.000000","reply_count":1},
			{"ts":"200.000000","user":"U1","text":"two"},
			{"ts":"100.000000","user":"U2","text":"one"}]}`),
		"/api/conversations.replies": fixed(`{"ok":true,"has_more":false,"messages":[
			{"ts":"300.000000","user":"U2","text":"three","thread_ts":"300.000000"},
			{"ts":"301.000000","user":"U1","text":"re","thread_ts":"300.000000"}]}`),
	})
	var res agentHistoryResult
	runJSON(t, f.session(t), "", &res, "history", "#general", "--until", "400", "--limit", "2", "--replies")
	if res.Channel.ID != "C1" || res.Channel.Name != "general" {
		t.Errorf("channel = %+v", res.Channel)
	}
	if !res.HasMore || len(res.Messages) != 2 || res.Messages[0].TS != "200.000000" || res.Messages[1].TS != "300.000000" {
		t.Fatalf("messages = %+v, has_more %v; want 200 then 300, more to come", res.Messages, res.HasMore)
	}
	if rs := res.Messages[1].Replies; len(rs) != 1 || rs[0].TS != "301.000000" || rs[0].UserName != "Me Myself" {
		t.Errorf("replies = %+v", rs)
	}
	if got := f.requests("/api/conversations.history")[0].form.Get("latest"); got != "400.000000" {
		t.Errorf("latest = %q", got)
	}
}

func TestAgentHistory_DMByHandle(t *testing.T) {
	f := newAgentFake(t, map[string]func(agentReq) string{
		"/api/users.conversations":   fixed(agentConversations),
		"/cache/T1/users/info":       agentUsersRoute(),
		"/cache/T1/users/search":     fixed(`{"ok":true,"results":[` + userBob + `]}`),
		"/api/conversations.history": fixed(`{"ok":true,"messages":[]}`),
	})
	var res agentHistoryResult
	runJSON(t, f.session(t), "", &res, "history", "@bob")
	if res.Channel.ID != "D1" || res.Channel.Type != "im" || res.Channel.Name != "Bobby" {
		t.Errorf("channel = %+v, want the DM with bob", res.Channel)
	}
	if res.Messages == nil {
		t.Error("messages is null; want []")
	}
}

func TestAgentHistory_NoDMIsNotFoundAndNothingIsOpened(t *testing.T) {
	f := newAgentFake(t, map[string]func(agentReq) string{
		"/api/users.conversations": fixed(`{"ok":true,"channels":[],"response_metadata":{"next_cursor":""}}`),
		"/cache/T1/users/search":   fixed(`{"ok":true,"results":[` + userBob + `]}`),
	})
	_, stderr, code := runAgentForTest(t, f.session(t), "", "history", "@bob")
	if code != 1 || decodeAgentError(t, stderr).Kind != "not_found" {
		t.Errorf("exit %d, %s", code, stderr)
	}
}

func TestAgentThread_FromAReplyPermalink(t *testing.T) {
	f := newAgentFake(t, map[string]func(agentReq) string{
		"/api/users.conversations": fixed(agentConversations),
		"/cache/T1/users/info":     agentUsersRoute(),
		"/api/conversations.replies": fixed(`{"ok":true,"has_more":false,"messages":[
			{"ts":"1700000300.000000","user":"U2","text":"parent","thread_ts":"1700000300.000000","reply_count":2},
			{"ts":"1700000302.000000","user":"U2","text":"second","thread_ts":"1700000300.000000"},
			{"ts":"1700000300.000000","user":"U2","text":"parent","thread_ts":"1700000300.000000"},
			{"ts":"1700000301.000000","user":"U1","text":"first","thread_ts":"1700000300.000000"}]}`),
	})
	var res agentThreadResult
	runJSON(t, f.session(t), "", &res, "thread", "https://acme.slack.com/archives/C1/p1700000301000000?thread_ts=1700000300.000000&cid=C1")
	if f.requests("/api/conversations.replies")[0].form.Get("ts") != "1700000300.000000" {
		t.Errorf("replies asked for %q, want the thread's parent", f.requests("/api/conversations.replies")[0].form.Get("ts"))
	}
	if res.Parent == nil || res.Parent.Text != "parent" || len(res.Replies) != 2 || res.Replies[0].TS != "1700000301.000000" {
		t.Errorf("thread = %+v", res)
	}
}

func TestAgentThread_OtherWorkspacePermalinkRefused(t *testing.T) {
	f := newAgentFake(t, nil)
	_, stderr, code := runAgentForTest(t, f.session(t), "", "thread", "https://other.slack.com/archives/C1/p1700000300000000")
	if code != 1 || decodeAgentError(t, stderr).Kind != "refused" {
		t.Errorf("exit %d, %s", code, stderr)
	}
}

const agentActivityFeed = `{"ok":true,"items":[
	{"is_unread":true,"feed_ts":"500.000000","key":"a","item":{"type":"at_user","message":{"channel":"C1","ts":"500.000000","author_user_id":"U2"}}},
	{"is_unread":false,"feed_ts":"450.000000","key":"b","item":{"type":"message_reaction","message":{"channel":"C1","ts":"450.000000"},"reaction":{"user":"U2","name":"tada"}}},
	{"is_unread":false,"feed_ts":"400.000000","key":"c","item":{"type":"at_user_group","message":{"channel":"C1","ts":"400.000000","thread_ts":"390.000000","author_user_id":"U2"}}},
	{"is_unread":false,"feed_ts":"100.000000","key":"d","item":{"type":"at_channel","message":{"channel":"C1","ts":"100.000000","author_user_id":"U2"}}}
],"response_metadata":{"next_cursor":"more"}}`

func TestAgentMentions_FiltersKindsStopsAtSinceAndHydrates(t *testing.T) {
	f := newAgentFake(t, map[string]func(agentReq) string{
		"/api/activity.feed": fixed(agentActivityFeed),
		"/api/messages.list": fixed(`{"ok":true,"messages_data":{"C1":{"messages":[
			{"ts":"500.000000","user":"U2","text":"hey <@U1>"},
			{"ts":"400.000000","user":"U2","text":"<!subteam^S1|@devs> look"}]}}}`),
		"/api/users.conversations": fixed(agentConversations),
		"/cache/T1/users/info":     agentUsersRoute(),
	})
	var res struct {
		Mentions []agentMention `json:"mentions"`
	}
	runJSON(t, f.session(t), "", &res, "mentions", "--since", "200")
	if len(res.Mentions) != 2 {
		t.Fatalf("mentions = %+v, want the user and usergroup mentions after --since", res.Mentions)
	}
	m := res.Mentions[0]
	if m.Kind != "user" || !m.Unread || m.TextPlain != "hey @Me Myself" || m.UserName != "Bobby" || m.ChannelName != "general" {
		t.Errorf("first = %+v", m)
	}
	if g := res.Mentions[1]; g.Kind != "usergroup" || g.ThreadTS != "390.000000" || g.TextPlain != "@devs look" {
		t.Errorf("second = %+v", g)
	}
	if n := len(f.requests("/api/activity.feed")); n != 1 {
		t.Errorf("activity.feed pages = %d; the feed passed --since on the first", n)
	}
}

// TestAgentMentions_GridMemberUsesTheSameUsersOrgSession: activity.feed
// answers team_is_restricted on a member token, so the feed comes off
// the org session of the same user, as in the TUI.
func TestAgentMentions_GridMemberUsesTheSameUsersOrgSession(t *testing.T) {
	member := newAgentFake(t, map[string]func(agentReq) string{
		"/api/auth.test":           fixed(`{"ok":true,"url":"https://acme-eng.enterprise.slack.com/","team":"Eng","user":"me","team_id":"T2","user_id":"U1","enterprise_id":"E1"}`),
		"/api/users.conversations": fixed(agentConversations),
		"/cache/T2/users/info":     agentUsersRoute(),
	})
	org := newAgentFake(t, map[string]func(agentReq) string{
		"/api/auth.test":     fixed(`{"ok":true,"url":"https://acme.enterprise.slack.com/","team":"Acme","user":"me","team_id":"E1","user_id":"U1","enterprise_id":"E1"}`),
		"/api/activity.feed": fixed(agentActivityFeed),
		"/api/messages.list": fixed(`{"ok":true,"messages_data":{}}`),
	})
	s := member.session(t)
	orgClient := newTestClient(t, org.Server)
	if err := orgClient.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.siblings = func(context.Context) []*slackclient.Client { return []*slackclient.Client{orgClient} }

	var res struct {
		Mentions []agentMention `json:"mentions"`
	}
	runJSON(t, s, "", &res, "mentions", "--limit", "1")
	if len(org.requests("/api/activity.feed")) != 1 || len(member.requests("/api/activity.feed")) != 0 {
		t.Errorf("activity.feed went to org %d times, member %d times; want org only",
			len(org.requests("/api/activity.feed")), len(member.requests("/api/activity.feed")))
	}
	if len(res.Mentions) != 1 || res.Mentions[0].ChannelID != "C1" {
		t.Errorf("mentions = %+v", res.Mentions)
	}
}

func TestAgentMentions_NonGridNeverConnectsSiblings(t *testing.T) {
	f := newAgentFake(t, map[string]func(agentReq) string{
		"/api/activity.feed":       fixed(`{"ok":true,"items":[]}`),
		"/api/users.conversations": fixed(agentConversations),
	})
	s := f.session(t)
	s.siblings = func(context.Context) []*slackclient.Client {
		t.Error("siblings connected for a non-Grid workspace")
		return nil
	}
	var res struct {
		Mentions []agentMention `json:"mentions"`
	}
	runJSON(t, s, "", &res, "mentions")
	if res.Mentions == nil {
		t.Error("mentions is null; want []")
	}
}
