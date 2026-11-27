// This file is the JSON the agent read commands print for users and
// messages, and the name resolution behind it. User IDs a command needs
// a name for are resolved in one batched edge users/info call (the
// TUI's revalidation endpoint), falling back to users.info per ID.

package main

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/gammons/slk/internal/export"
	"github.com/gammons/slk/internal/ids"
	slackclient "github.com/gammons/slk/internal/slack"
	"github.com/gammons/slk/internal/slack/edge"
	"github.com/gammons/slk/internal/slackfmt"
	"github.com/gammons/slk/internal/slackurl"
	"github.com/gammons/slk/internal/ui/messages"
	"github.com/slack-go/slack"
)

// agentUserFallbackLimit caps the users.info calls one command makes
// when edge users/info cannot answer: one request per user, so a
// command touching hundreds of authors must not turn into a sweep.
const agentUserFallbackLimit = 50

// agentUser is one user as the agent commands print them.
type agentUser struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	RealName    string `json:"real_name"`
	Title       string `json:"title"`
	TZ          string `json:"tz"`
	IsBot       bool   `json:"is_bot"`
	Deleted     bool   `json:"deleted"`
	TeamID      string `json:"team_id,omitempty"`
}

// shownName is the name slk shows for a user: display name, then real
// name, then handle (profileDisplayName's order).
func (u agentUser) shownName() string {
	for _, n := range []string{u.DisplayName, u.RealName, u.Name} {
		if n != "" {
			return n
		}
	}
	return u.ID
}

func agentUserFromEdge(u edge.User) agentUser {
	real := u.Profile.RealName
	if real == "" {
		real = u.RealName
	}
	return agentUser{
		ID: u.ID, Name: u.Name, DisplayName: u.Profile.DisplayName, RealName: real,
		Title: u.Profile.Title, TZ: u.TZ, IsBot: u.IsBot, Deleted: u.Deleted, TeamID: u.TeamID,
	}
}

func agentUserFromProfile(p *slackclient.UserProfile) agentUser {
	return agentUser{
		ID: p.ID, Name: p.Name, DisplayName: p.Profile.DisplayName, RealName: p.RealName,
		Title: p.Profile.Title, TZ: p.TZ, IsBot: p.IsBot, Deleted: p.Deleted, TeamID: p.TeamID,
	}
}

// rememberUser records u for this run, including in the name store
// exportMessageItem reads authors from.
func (s *agentSession) rememberUser(u agentUser) {
	s.users[u.ID] = u
	s.wctx.UserNames.Set(u.ID, u.shownName())
}

// resolveUsers makes sure every user ID in userIDs that this run has
// not seen is looked up. IDs Slack cannot resolve stay unknown; their
// messages show the raw ID, as in the TUI.
func (s *agentSession) resolveUsers(ctx context.Context, userIDs []string) {
	want := map[string]int64{}
	for _, id := range userIDs {
		if _, known := s.users[id]; known || !looksLikeUserID(id) {
			continue
		}
		want[id] = 0 // version 0: "never seen", so edge returns it
	}
	if len(want) == 0 {
		return
	}
	if s.edge != nil {
		if found, err := s.edge.UsersInfo(ctx, want); err == nil {
			for _, u := range found {
				s.rememberUser(agentUserFromEdge(u))
				delete(want, u.ID)
			}
		}
	}
	missing := make([]string, 0, len(want))
	for id := range want {
		missing = append(missing, id)
	}
	sort.Strings(missing)
	if len(missing) > agentUserFallbackLimit {
		missing = missing[:agentUserFallbackLimit]
	}
	for _, id := range missing {
		var p *slackclient.UserProfile
		err := slackclient.RetryRateLimited(ctx, func() error {
			var err error
			p, err = s.client.GetUserProfileContext(ctx, id)
			return err
		})
		if err == nil && p != nil && p.User != nil {
			s.rememberUser(agentUserFromProfile(p))
		}
	}
}

// looksLikeUserID is true for U… and W… (Grid) user IDs; bot IDs and
// empty strings have no users.info answer.
func looksLikeUserID(id string) bool {
	return len(id) > 1 && (id[0] == 'U' || id[0] == 'W')
}

// userName is a resolver for messages.FlattenMrkdwn.
func (s *agentSession) userName(id string) (string, bool) {
	if u, ok := s.users[id]; ok {
		return u.shownName(), true
	}
	return "", false
}

// channelName is a resolver for messages.FlattenMrkdwn: the names of
// the joined conversations, when this run has loaded them.
func (s *agentSession) channelName(id string) (string, bool) {
	for _, ch := range s.joined {
		if ch.ID == id && ch.Name != "" {
			return ch.Name, true
		}
	}
	return "", false
}

// permalink builds a message's link the way Slack's client does, with
// no API call (slackurl.Build); "" if the workspace URL is unknown.
func (s *agentSession) permalink(channelID, ts, threadTS string) string {
	link, _ := slackurl.Build(s.client.TeamURL(), ids.ChannelID(channelID), ids.MessageTS(ts), ids.ThreadTS(threadTS))
	return link
}

// agentMessage is one message as the agent commands print them. Text is
// Slack's mrkdwn as sent (mentions as <@U…>); TextPlain is the same
// with mentions, links and entities flattened to what a reader sees,
// including a forwarded message's shared text.
type agentMessage struct {
	ChannelID   string          `json:"channel_id"`
	ChannelName string          `json:"channel_name,omitempty"`
	ChannelType string          `json:"channel_type,omitempty"`
	TS          string          `json:"ts"`
	ThreadTS    string          `json:"thread_ts,omitempty"`
	Time        string          `json:"time"`
	UserID      string          `json:"user_id,omitempty"`
	UserName    string          `json:"user_name,omitempty"`
	UserHandle  string          `json:"user_handle,omitempty"`
	BotID       string          `json:"bot_id,omitempty"`
	Subtype     string          `json:"subtype,omitempty"`
	Text        string          `json:"text"`
	TextPlain   string          `json:"text_plain"`
	ReplyCount  int             `json:"reply_count,omitempty"`
	Edited      bool            `json:"edited,omitempty"`
	Reactions   []agentReaction `json:"reactions,omitempty"`
	Files       []agentFile     `json:"files,omitempty"`
	Permalink   string          `json:"permalink,omitempty"`
	Replies     []agentMessage  `json:"replies,omitempty"`
}

type agentReaction struct {
	Name  string   `json:"name"`
	Count int      `json:"count"`
	Users []string `json:"users"`
}

type agentFile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Title     string `json:"title,omitempty"`
	Mimetype  string `json:"mimetype,omitempty"`
	URL       string `json:"url_private,omitempty"`
	Permalink string `json:"permalink,omitempty"`
}

// agentSource is a message and the conversation it is in.
type agentSource struct {
	channelID string
	msg       slack.Message
}

// toAgentMessages converts msgs, resolving every author and mentioned
// user first, in one batch.
func (s *agentSession) toAgentMessages(ctx context.Context, srcs []agentSource) []agentMessage {
	var need []string
	for _, src := range srcs {
		need = append(need, src.msg.User)
		item := exportMessageItem(src.msg, s.wctx.UserNames, nil)
		need = append(need, slackfmt.MentionedUserIDs(messages.MessageCopyText(item))...)
	}
	s.resolveUsers(ctx, need)
	out := make([]agentMessage, 0, len(srcs))
	for _, src := range srcs {
		out = append(out, s.agentMessageFrom(src.channelID, src.msg))
	}
	return out
}

// agentMessageFrom converts one message whose users are resolved.
func (s *agentSession) agentMessageFrom(channelID string, m slack.Message) agentMessage {
	item := exportMessageItem(m, s.wctx.UserNames, nil)
	out := agentMessage{
		ChannelID:  channelID,
		TS:         m.Timestamp,
		ThreadTS:   m.ThreadTimestamp,
		Time:       agentTime(m.Timestamp),
		UserID:     m.User,
		UserName:   item.UserName,
		BotID:      m.BotID,
		Subtype:    m.SubType,
		Text:       m.Text,
		TextPlain:  messages.FlattenMrkdwnWithUserGroups(messages.MessageCopyText(item), s.userName, s.channelName, s.userGroups),
		ReplyCount: m.ReplyCount,
		Edited:     m.Edited != nil,
		Permalink:  s.permalink(channelID, m.Timestamp, m.ThreadTimestamp),
	}
	if u, ok := s.users[m.User]; ok {
		out.UserHandle = u.Name
	}
	for _, r := range m.Reactions {
		out.Reactions = append(out.Reactions, agentReaction{Name: r.Name, Count: r.Count, Users: r.Users})
	}
	for _, f := range m.Files {
		out.Files = append(out.Files, agentFile{ID: f.ID, Name: f.Name, Title: f.Title, Mimetype: f.Mimetype, URL: f.URLPrivate, Permalink: f.Permalink})
	}
	return out
}

// agentTime is a Slack ts as an RFC 3339 local time, "" if malformed.
func agentTime(ts string) string {
	t, ok := export.TimeFromTS(ts)
	if !ok {
		return ""
	}
	return t.Local().Format(time.RFC3339)
}

// agentChannelType names a conversation's kind the way --type does.
func agentChannelType(ch slack.Channel) string {
	switch {
	case ch.IsIM:
		return "im"
	case ch.IsMpIM:
		return "mpim"
	case ch.IsPrivate || ch.IsGroup:
		return "private"
	}
	return "public"
}

// conversationName is what a conversation is called: its name, the
// peer's name for a DM, the members' names for a group DM.
func (s *agentSession) conversationName(ch slack.Channel) string {
	switch {
	case ch.IsIM:
		if u, ok := s.users[ch.User]; ok {
			return u.shownName()
		}
		return ch.User
	case ch.IsMpIM:
		handles := map[string]string{}
		for _, u := range s.users {
			handles[u.Name] = u.shownName()
		}
		return slackfmt.FormatMPDMName(ch.Name, s.client.UserName(), func(handle string) string { return handles[handle] })
	}
	return ch.Name
}

// splitList reads a comma-separated flag value, dropping blanks.
func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
