// This file holds the agent read commands: search, channels, users,
// history, thread and mentions. Each parses its flags before it
// connects, so a usage error never costs a network call.

package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/gammons/slk/internal/core"
	slackclient "github.com/gammons/slk/internal/slack"
	"github.com/gammons/slk/internal/slackurl"
	"github.com/slack-go/slack"
)

// agentChannelIDRe matches a conversation ID: C… channels, G… private
// channels and group DMs, D… DMs.
var agentChannelIDRe = regexp.MustCompile(`^[CDG][A-Z0-9]{6,}$`)

// resolveAgentChannel finds the conversation want names: a permalink,
// a conversation ID, a channel name (with or without #), or a person
// (@handle or user ID) whose DM already exists. A name or person only
// matches a conversation the user is in; an ID the user is not in is
// looked up with conversations.info (via the Grid org session when
// this is a member workspace, as the TUI does).
func (s *agentSession) resolveAgentChannel(ctx context.Context, want string) (slack.Channel, error) {
	want = strings.TrimSpace(want)
	if want == "" {
		return slack.Channel{}, agentUsageError("a channel is required")
	}
	if pl, ok := slackurl.Parse(want); ok {
		if err := s.checkPermalinkWorkspace(pl.Subdomain); err != nil {
			return slack.Channel{}, err
		}
		want = string(pl.ChannelID)
	}
	joined, err := s.joinedConversations(ctx)
	if err != nil {
		return slack.Channel{}, err
	}
	if strings.HasPrefix(want, "@") || userIDShapeRe.MatchString(want) {
		return s.findDM(ctx, joined, want)
	}
	if agentChannelIDRe.MatchString(want) {
		for _, ch := range joined {
			if ch.ID == want {
				return ch, nil
			}
		}
		s.orgClient(ctx) // connects the Grid siblings the resolver may need
		ch, err := conversationResolverFor(s.router, s.wctx)(ctx, want)
		if err != nil {
			return slack.Channel{}, agentNotFound("conversation %s: %v", want, err)
		}
		return *ch, nil
	}
	if ch, ok := findExportChannel(joined, want); ok {
		return ch, nil
	}
	return slack.Channel{}, agentNotFound("no conversation you are in is called %q", want)
}

// findDM finds the existing DM with the person who (an @handle or a
// user ID). It never opens one: no command has a default target.
func (s *agentSession) findDM(ctx context.Context, joined []slack.Channel, who string) (slack.Channel, error) {
	userID := who
	if handle, ok := strings.CutPrefix(who, "@"); ok {
		u, err := s.findUserByHandle(ctx, handle)
		if err != nil {
			return slack.Channel{}, err
		}
		userID = u.ID
	}
	for _, ch := range joined {
		if ch.IsIM && ch.User == userID {
			s.resolveUsers(ctx, []string{userID})
			return ch, nil
		}
	}
	return slack.Channel{}, agentNotFound("you have no open DM with %s", who)
}

// findUserByHandle finds the user whose handle (or display name) is
// exactly handle, case-insensitively, through edge users/search. The
// signed-in user is known without a request.
func (s *agentSession) findUserByHandle(ctx context.Context, handle string) (agentUser, error) {
	if strings.EqualFold(handle, s.client.UserName()) {
		s.resolveUsers(ctx, []string{s.client.UserID()})
		if u, ok := s.users[s.client.UserID()]; ok {
			return u, nil
		}
		return agentUser{ID: s.client.UserID(), Name: s.client.UserName()}, nil
	}
	found, err := s.edge.UsersSearch(ctx, handle, "", nil)
	if err != nil {
		return agentUser{}, fmt.Errorf("looking up @%s: %w", handle, err)
	}
	for _, byDisplay := range []bool{false, true} {
		for _, eu := range found {
			u := agentUserFromEdge(eu)
			name := u.Name
			if byDisplay {
				name = u.DisplayName
			}
			if strings.EqualFold(name, handle) {
				s.rememberUser(u)
				return u, nil
			}
		}
	}
	return agentUser{}, agentNotFound("no user has the handle @%s", handle)
}

// agentChannelRef names the conversation a command read.
type agentChannelRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

func (s *agentSession) channelRef(ch slack.Channel) agentChannelRef {
	return agentChannelRef{ID: ch.ID, Name: s.conversationName(ch), Type: agentChannelType(ch)}
}

// agentSearchResult is what `slk search` prints.
type agentSearchResult struct {
	Query   string         `json:"query"`
	Total   int            `json:"total"`
	Page    int            `json:"page"`
	Pages   int            `json:"pages"`
	Count   int            `json:"count"`
	Matches []agentMessage `json:"matches"`
}

func runAgentSearch(env *agentEnv, args []string) (any, error) {
	fs, workspace := newAgentFlags(env, "search")
	count := fs.Int("count", 20, "results per page, 1-100")
	page := fs.Int("page", 1, "page of results, from 1")
	sortBy := fs.String("sort", "score", "score (relevance) or timestamp")
	sortDir := fs.String("sort-dir", "desc", "desc or asc")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, err
	}
	query := strings.TrimSpace(strings.Join(pos, " "))
	switch {
	case query == "":
		return nil, agentUsageError("a search query is required")
	case *count < 1 || *count > 100:
		return nil, agentUsageError("--count must be between 1 and 100")
	case *page < 1:
		return nil, agentUsageError("--page must be 1 or more")
	case *sortBy != "score" && *sortBy != "timestamp":
		return nil, agentUsageError("--sort must be score or timestamp")
	case *sortDir != "desc" && *sortDir != "asc":
		return nil, agentUsageError("--sort-dir must be desc or asc")
	}
	s, err := env.open(env.ctx, *workspace, "")
	if err != nil {
		return nil, err
	}
	var res *slack.SearchMessages
	err = slackclient.RetryRateLimited(env.ctx, func() error {
		var err error
		res, err = s.client.SearchMessagesPage(env.ctx, query, slackclient.SearchOptions{Count: *count, Page: *page, Sort: *sortBy, SortDir: *sortDir})
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.searchResult(env.ctx, query, res), nil
}

// searchResult converts search.messages matches. A DM hit's channel
// "name" is the peer's user ID on the wire; it is resolved to a name.
func (s *agentSession) searchResult(ctx context.Context, query string, res *slack.SearchMessages) agentSearchResult {
	srcs := make([]agentSource, 0, len(res.Matches))
	var peers []string
	for _, m := range res.Matches {
		srcs = append(srcs, agentSource{channelID: m.Channel.ID, msg: slack.Message{Msg: slack.Msg{
			User: m.User, Username: m.Username, Timestamp: m.Timestamp, Text: m.Text,
			Attachments: m.Attachments, Blocks: m.Blocks,
		}}})
		if userIDShapeRe.MatchString(m.Channel.Name) {
			peers = append(peers, m.Channel.Name)
		}
	}
	s.resolveUsers(ctx, peers)
	out := s.toAgentMessages(ctx, srcs)
	for i, m := range res.Matches {
		msg := &out[i]
		msg.ChannelName = m.Channel.Name
		if name, ok := s.userName(m.Channel.Name); ok {
			msg.ChannelName = name
		}
		switch {
		case strings.HasPrefix(m.Channel.ID, "D"):
			msg.ChannelType = "im"
		case m.Channel.IsMPIM:
			msg.ChannelType = "mpim"
		case m.Channel.IsPrivate:
			msg.ChannelType = "private"
		default:
			msg.ChannelType = "public"
		}
		if m.Permalink != "" {
			msg.Permalink = m.Permalink
		}
		// Search hits carry no thread_ts; a reply's permalink does.
		if pl, ok := slackurl.Parse(m.Permalink); ok {
			msg.ThreadTS = string(pl.ThreadTS)
		}
		if msg.UserHandle == "" {
			msg.UserHandle = m.Username
		}
		if msg.UserName == msg.UserID && m.Username != "" {
			msg.UserName = m.Username
		}
	}
	return agentSearchResult{
		Query: query, Total: res.Total, Page: res.Paging.Page, Pages: res.Pages,
		Count: len(out), Matches: out,
	}
}

// errStopWalk ends a WalkHistory early once enough messages are in.
var errStopWalk = errors.New("enough messages")

// agentChannel is one conversation as `slk channels` prints it.
type agentChannel struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Topic       string   `json:"topic,omitempty"`
	Purpose     string   `json:"purpose,omitempty"`
	MemberCount *int     `json:"member_count,omitempty"`
	UserID      string   `json:"user_id,omitempty"`
	UserName    string   `json:"user_name,omitempty"`
	UserHandle  string   `json:"user_handle,omitempty"`
	IsSelf      bool     `json:"is_self,omitempty"`
	Members     []string `json:"members,omitempty"`
}

var agentChannelTypes = []string{"public", "private", "mpim", "im"}

func runAgentChannels(env *agentEnv, args []string) (any, error) {
	fs, workspace := newAgentFlags(env, "channels")
	types := fs.String("type", strings.Join(agentChannelTypes, ","), "comma-separated kinds: public, private, mpim, im")
	memberCount := fs.Bool("member-count", false, "also count the members of each channel (one request per channel)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, agentUsageError("unexpected argument %q", pos[0])
	}
	want := map[string]bool{}
	for _, t := range splitList(*types) {
		if !slices.Contains(agentChannelTypes, t) {
			return nil, agentUsageError("--type %q: want one of %s", t, strings.Join(agentChannelTypes, ", "))
		}
		want[t] = true
	}
	if len(want) == 0 {
		return nil, agentUsageError("--type is empty")
	}
	s, err := env.open(env.ctx, *workspace, "")
	if err != nil {
		return nil, err
	}
	joined, err := s.joinedConversations(env.ctx)
	if err != nil {
		return nil, err
	}
	var picked []slack.Channel
	var people []string
	for _, ch := range joined {
		if !want[agentChannelType(ch)] {
			continue
		}
		picked = append(picked, ch)
		if ch.IsIM {
			people = append(people, ch.User)
		}
		if ch.IsMpIM {
			people = append(people, ch.Members...)
		}
	}
	s.resolveUsers(env.ctx, people)
	out := make([]agentChannel, 0, len(picked))
	for _, ch := range picked {
		out = append(out, s.agentChannelFrom(env.ctx, ch, *memberCount))
	}
	sortAgentChannels(out)
	return struct {
		Channels []agentChannel `json:"channels"`
	}{out}, nil
}

// agentChannelFrom converts one joined conversation. The member count
// is free for DMs (1 or 2) and group DMs (their member list); for a
// channel it costs an edge users/counts call, so only when asked.
func (s *agentSession) agentChannelFrom(ctx context.Context, ch slack.Channel, countMembers bool) agentChannel {
	out := agentChannel{
		ID: ch.ID, Name: s.conversationName(ch), Type: agentChannelType(ch),
		Topic: ch.Topic.Value, Purpose: ch.Purpose.Value,
	}
	switch {
	case ch.IsIM:
		out.UserID = ch.User
		out.IsSelf = ch.User == s.client.UserID()
		if u, ok := s.users[ch.User]; ok {
			out.UserName, out.UserHandle = u.shownName(), u.Name
		}
		n := 2
		if out.IsSelf {
			n = 1
		}
		out.MemberCount = &n
	case ch.IsMpIM:
		out.Members = ch.Members
		if n := len(ch.Members); n > 0 {
			out.MemberCount = &n
		}
	case countMembers:
		if counts, err := s.edge.UsersCounts(ctx, ch.ID); err == nil {
			n := counts.Everyone
			out.MemberCount = &n
		}
	}
	return out
}

// sortAgentChannels orders channels as --type lists the kinds, then
// by name.
func sortAgentChannels(chs []agentChannel) {
	rank := map[string]int{}
	for i, t := range agentChannelTypes {
		rank[t] = i
	}
	slices.SortStableFunc(chs, func(a, b agentChannel) int {
		if rank[a.Type] != rank[b.Type] {
			return rank[a.Type] - rank[b.Type]
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
}

func runAgentUsers(env *agentEnv, args []string) (any, error) {
	fs, workspace := newAgentFlags(env, "users")
	query := fs.String("query", "", "find users by name, handle or email (server-ranked, at most 30)")
	idList := fs.String("id", "", "comma-separated user IDs to look up")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, agentUsageError("unexpected argument %q", pos[0])
	}
	userIDs := splitList(*idList)
	if strings.TrimSpace(*query) == "" && len(userIDs) == 0 {
		return nil, agentUsageError("give --query or --id: slk does not list every user in the workspace (that walk is what gets Grid accounts signed out for scraping)")
	}
	s, err := env.open(env.ctx, *workspace, "")
	if err != nil {
		return nil, err
	}
	out := []agentUser{}
	seen := map[string]bool{}
	if q := strings.TrimSpace(*query); q != "" {
		found, err := s.edge.UsersSearch(env.ctx, q, "", nil)
		if err != nil {
			return nil, err
		}
		for _, eu := range found {
			u := agentUserFromEdge(eu)
			s.rememberUser(u)
			out = append(out, u)
			seen[u.ID] = true
		}
	}
	s.resolveUsers(env.ctx, userIDs)
	var notFound []string
	for _, id := range userIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		if u, ok := s.users[id]; ok {
			out = append(out, u)
		} else {
			notFound = append(notFound, id)
		}
	}
	return struct {
		Users    []agentUser `json:"users"`
		NotFound []string    `json:"not_found,omitempty"`
	}{out, notFound}, nil
}

// agentHistoryResult is what `slk history` prints.
type agentHistoryResult struct {
	Channel  agentChannelRef `json:"channel"`
	Messages []agentMessage  `json:"messages"`
	HasMore  bool            `json:"has_more"`
}

func runAgentHistory(env *agentEnv, args []string) (any, error) {
	fs, workspace := newAgentFlags(env, "history")
	since := fs.String("since", "", "oldest time to include: YYYY-MM-DD, RFC 3339, a Slack ts, or 36h / 7d ago")
	until := fs.String("until", "", "time to stop before (exclusive); same forms as --since")
	limit := fs.Int("limit", 50, "most messages to return (the newest in range), 1-1000")
	replies := fs.Bool("replies", false, "include each thread's replies under its parent")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 1 {
		return nil, agentUsageError("history takes one channel")
	}
	if *limit < 1 || *limit > 1000 {
		return nil, agentUsageError("--limit must be between 1 and 1000")
	}
	oldest, latest, err := agentRange(*since, *until)
	if err != nil {
		return nil, err
	}
	s, err := env.open(env.ctx, *workspace, permalinkHint(pos[0]))
	if err != nil {
		return nil, err
	}
	ch, err := s.resolveAgentChannel(env.ctx, pos[0])
	if err != nil {
		return nil, err
	}
	var got []slack.Message
	err = s.client.WalkHistory(env.ctx, ch.ID, oldest, latest, func(page []slack.Message) error {
		for _, m := range page {
			// WalkHistory's latest is inclusive; --until is not.
			if latest != "" && compareSlackTS(m.Timestamp, latest) >= 0 {
				continue
			}
			got = append(got, m)
			if len(got) > *limit {
				return errStopWalk
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		return nil, err
	}
	hasMore := len(got) > *limit
	if hasMore {
		got = got[:*limit]
	}
	slices.Reverse(got) // Slack pages newest first; print oldest first
	srcs := make([]agentSource, len(got))
	for i, m := range got {
		srcs[i] = agentSource{channelID: ch.ID, msg: m}
	}
	out := s.toAgentMessages(env.ctx, srcs)
	if *replies {
		for i, m := range got {
			if m.ReplyCount == 0 || (m.ThreadTimestamp != "" && m.ThreadTimestamp != m.Timestamp) {
				continue
			}
			_, rs, err := s.fetchThread(env.ctx, ch.ID, m.Timestamp)
			if err != nil {
				return nil, err
			}
			out[i].Replies = rs
		}
	}
	return agentHistoryResult{Channel: s.channelRef(ch), Messages: out, HasMore: hasMore}, nil
}

// agentRange reads --since and --until into Slack ts bounds.
func agentRange(since, until string) (string, string, error) {
	now := agentNow()
	var oldest, latest string
	var err error
	if since != "" {
		if oldest, err = parseAgentTS(since, now); err != nil {
			return "", "", agentUsageError("--since: %v", err)
		}
	}
	if until != "" {
		if latest, err = parseAgentTS(until, now); err != nil {
			return "", "", agentUsageError("--until: %v", err)
		}
	}
	if oldest != "" && latest != "" && compareSlackTS(oldest, latest) >= 0 {
		return "", "", agentUsageError("--since must be before --until")
	}
	return oldest, latest, nil
}

// permalinkHint is a permalink's workspace subdomain, for picking the
// workspace when none is named; "" for anything else.
func permalinkHint(arg string) string {
	if pl, ok := slackurl.Parse(arg); ok {
		return pl.Subdomain
	}
	return ""
}

// fetchThread returns a thread's parent and its replies, oldest first,
// each once (Slack may repeat the parent on every page).
func (s *agentSession) fetchThread(ctx context.Context, channelID, threadTS string) (*agentMessage, []agentMessage, error) {
	msgs, err := s.client.GetRepliesBetween(ctx, channelID, threadTS, "", "")
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	var parent *slack.Message
	var replies []slack.Message
	for i := range msgs {
		m := msgs[i]
		if seen[m.Timestamp] {
			continue
		}
		seen[m.Timestamp] = true
		if m.Timestamp == threadTS {
			parent = &msgs[i]
			continue
		}
		replies = append(replies, m)
	}
	slices.SortFunc(replies, compareMessagesByTS)
	srcs := make([]agentSource, 0, len(replies)+1)
	if parent != nil {
		srcs = append(srcs, agentSource{channelID: channelID, msg: *parent})
	}
	for _, m := range replies {
		srcs = append(srcs, agentSource{channelID: channelID, msg: m})
	}
	out := s.toAgentMessages(ctx, srcs)
	if parent == nil {
		return nil, out, nil
	}
	return &out[0], out[1:], nil
}

// agentThreadResult is what `slk thread` prints.
type agentThreadResult struct {
	Channel  agentChannelRef `json:"channel"`
	ThreadTS string          `json:"thread_ts"`
	Parent   *agentMessage   `json:"parent"`
	Replies  []agentMessage  `json:"replies"`
}

func runAgentThread(env *agentEnv, args []string) (any, error) {
	fs, workspace := newAgentFlags(env, "thread")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, err
	}
	var channelArg, ts string
	switch len(pos) {
	case 1:
		pl, ok := slackurl.Parse(pos[0])
		if !ok {
			return nil, agentUsageError("%q is not a Slack message permalink; give a permalink, or a channel and a ts", pos[0])
		}
		channelArg, ts = pos[0], string(pl.MessageTS)
		if pl.ThreadTS != "" {
			ts = string(pl.ThreadTS)
		}
	case 2:
		if !agentTSRe.MatchString(pos[1]) {
			return nil, agentUsageError("%q is not a Slack ts", pos[1])
		}
		channelArg, ts = pos[0], pos[1]
	default:
		return nil, agentUsageError("thread takes a permalink, or a channel and a ts")
	}
	s, err := env.open(env.ctx, *workspace, permalinkHint(channelArg))
	if err != nil {
		return nil, err
	}
	ch, err := s.resolveAgentChannel(env.ctx, channelArg)
	if err != nil {
		return nil, err
	}
	parent, replies, err := s.fetchThread(env.ctx, ch.ID, ts)
	if err != nil {
		return nil, err
	}
	// A reply's ts names its thread only through the reply: fetch again
	// from the thread's own parent.
	if parent == nil && len(replies) > 0 && replies[0].ThreadTS != "" && replies[0].ThreadTS != ts {
		ts = replies[0].ThreadTS
		if parent, replies, err = s.fetchThread(env.ctx, ch.ID, ts); err != nil {
			return nil, err
		}
	}
	if parent == nil {
		return nil, agentNotFound("no message %s in %s", ts, ch.ID)
	}
	if replies == nil {
		replies = []agentMessage{}
	}
	return agentThreadResult{Channel: s.channelRef(ch), ThreadTS: ts, Parent: parent, Replies: replies}, nil
}

// agentMentionKinds maps the Activity feed types that are mentions to
// the kind `slk mentions` prints. unjoined_channel_mention is an
// @-mention in a channel the user is not in.
var agentMentionKinds = map[string]string{
	"at_user":                  "user",
	"unjoined_channel_mention": "user",
	"at_user_group":            "usergroup",
	"at_channel":               "channel",
	"at_everyone":              "everyone",
}

const (
	agentActivityPage     = 50 // what the TUI's Activity view asks for
	agentActivityMaxPages = 20
)

// agentMention is one mention: the message, plus what kind of mention
// it is and whether the Activity feed has it unread.
type agentMention struct {
	Kind         string `json:"kind"`
	ActivityType string `json:"activity_type"`
	Unread       bool   `json:"unread"`
	agentMessage
}

func runAgentMentions(env *agentEnv, args []string) (any, error) {
	fs, workspace := newAgentFlags(env, "mentions")
	since := fs.String("since", "", "oldest mention to include: YYYY-MM-DD, RFC 3339, a Slack ts, or 36h / 7d ago")
	limit := fs.Int("limit", 50, "most mentions to return, newest first, 1-500")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, agentUsageError("unexpected argument %q", pos[0])
	}
	if *limit < 1 || *limit > 500 {
		return nil, agentUsageError("--limit must be between 1 and 500")
	}
	oldest, _, err := agentRange(*since, "")
	if err != nil {
		return nil, err
	}
	s, err := env.open(env.ctx, *workspace, "")
	if err != nil {
		return nil, err
	}
	items, err := s.mentionItems(env.ctx, oldest, *limit)
	if err != nil {
		return nil, err
	}
	return struct {
		Mentions []agentMention `json:"mentions"`
	}{s.hydrateMentions(env.ctx, items)}, nil
}

// mentionItems pages the Activity feed (newest first) for mention
// items no older than oldest, up to limit. On a Grid member workspace
// the feed comes off the org session and is org-wide, so it is cut to
// this workspace's conversations as the TUI cuts it.
func (s *agentSession) mentionItems(ctx context.Context, oldest string, limit int) ([]slackclient.ActivityItem, error) {
	client := s.orgClient(ctx)
	orgWide := client != s.client
	if orgWide {
		if _, err := s.joinedConversations(ctx); err != nil {
			return nil, err
		}
	}
	var out []slackclient.ActivityItem
	cursor := ""
	for page := 0; page < agentActivityMaxPages; page++ {
		var res slackclient.ActivityFeedResult
		err := slackclient.RetryRateLimited(ctx, func() error {
			var err error
			res, err = client.GetActivityFeed(ctx, agentActivityPage, cursor, false)
			return err
		})
		if err != nil {
			return nil, err
		}
		items := res.Items
		if orgWide {
			items = filterActivityItemsToWorkspace(items, s.wctx)
		}
		older := false
		for _, it := range items {
			if oldest != "" && compareSlackTS(it.FeedTS, oldest) < 0 {
				older = true
				continue
			}
			if _, ok := agentMentionKinds[it.Type]; !ok || it.ChannelID == "" || it.TS == "" {
				continue
			}
			out = append(out, it)
			if len(out) == limit {
				return out, nil
			}
		}
		if older || res.NextCursor == "" {
			return out, nil
		}
		cursor = res.NextCursor
	}
	return out, nil
}

// hydrateMentions fetches the mentioned messages' bodies (messages.list,
// as the Activity view does) and converts them. A body Slack does not
// return leaves text empty rather than dropping the mention.
func (s *agentSession) hydrateMentions(ctx context.Context, items []slackclient.ActivityItem) []agentMention {
	if len(items) == 0 {
		return []agentMention{}
	}
	refs := map[string][]string{}
	for _, it := range items {
		refs[it.ChannelID] = append(refs[it.ChannelID], it.TS)
	}
	var bodies map[string]slackclient.ActivityMessage
	client := s.orgClient(ctx)
	_ = slackclient.RetryRateLimited(ctx, func() error {
		var err error
		bodies, err = client.GetActivityMessages(ctx, refs)
		return err
	})
	// Channel names, best effort: a failure leaves IDs.
	joined, _ := s.joinedConversations(ctx)
	var peers []string
	for _, ch := range joined {
		if ch.IsIM {
			peers = append(peers, ch.User)
		}
	}
	srcs := make([]agentSource, 0, len(items))
	for _, it := range items {
		body := bodies[core.ActivityMsgKey(it.ChannelID, it.TS)]
		user := body.UserID
		if user == "" {
			user = it.AuthorID
		}
		srcs = append(srcs, agentSource{channelID: it.ChannelID, msg: slack.Message{Msg: slack.Msg{
			User: user, Text: body.Text, Timestamp: it.TS, ThreadTimestamp: it.ThreadTS,
		}}})
	}
	s.resolveUsers(ctx, peers)
	msgs := s.toAgentMessages(ctx, srcs)
	out := make([]agentMention, 0, len(items))
	for i, it := range items {
		m := msgs[i]
		for _, ch := range joined {
			if ch.ID == it.ChannelID {
				m.ChannelName, m.ChannelType = s.conversationName(ch), agentChannelType(ch)
				break
			}
		}
		out = append(out, agentMention{Kind: agentMentionKinds[it.Type], ActivityType: it.Type, Unread: it.IsUnread, agentMessage: m})
	}
	return out
}
