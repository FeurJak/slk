// This file holds the agent write commands: send, reply, react, edit
// and delete. Text goes through the composer's own mention translation
// (compose.Model.TranslateMentionsForSend), reference linkifying
// (reflink.Linkify, unless --no-linkify) and then the same conversion
// every send uses (slackclient.ConvertMessage). --dry-run
// prints the payload and sends nothing. No command has a default
// target: the channel or permalink is always spelled out.

package main

import (
	"context"
	"regexp"
	"strings"

	"github.com/gammons/slk/internal/bubbles/channelpicker"
	"github.com/gammons/slk/internal/reflink"
	slackclient "github.com/gammons/slk/internal/slack"
	"github.com/gammons/slk/internal/slackurl"
	"github.com/gammons/slk/internal/ui/compose"
	"github.com/gammons/slk/internal/ui/mentionpicker"
	"github.com/slack-go/slack"
)

// agentAtWordRe finds the @words in text that could be mentions: an @
// at the start or after a character that cannot be part of an address
// or a wire token, so "a@b.com" and "<@U1>" are left alone.
var agentAtWordRe = regexp.MustCompile(`(?:^|[^\p{L}\p{N}_.@<|/])@([\p{L}\p{N}][\p{L}\p{N}._'-]*)`)

// agentMaxMentionLookups caps the users/search calls one message makes.
const agentMaxMentionLookups = 10

var agentSpecialMentions = map[string]bool{"here": true, "channel": true, "everyone": true}

// atWords returns the distinct @words of text that are not @here,
// @channel or @everyone, trailing punctuation trimmed, in order.
func atWords(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range agentAtWordRe.FindAllStringSubmatch(text, -1) {
		w := strings.TrimRight(m[1], ".-'")
		key := strings.ToLower(w)
		if w == "" || agentSpecialMentions[key] || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, w)
	}
	return out
}

// translateMentions turns the @name and #channel mentions a person
// types into Slack's wire form, with the composer's own translation.
// The composer is given only what text can refer to: each @word looked
// up with edge users/search (registered under display name, handle and
// real name), the usergroups, and the joined channels when text has a
// #. unresolved lists the @words left as plain text.
func (s *agentSession) translateMentions(ctx context.Context, text string) (wire string, unresolved []string, err error) {
	words := atWords(text)
	box := compose.New("")
	if len(words) > 0 {
		var users []mentionpicker.User
		addUser := func(u agentUser) {
			seen := map[string]bool{}
			for _, name := range []string{u.DisplayName, u.Name, u.RealName} {
				if name != "" && !seen[name] {
					seen[name] = true
					users = append(users, mentionpicker.User{ID: u.ID, DisplayName: name, Username: u.Name, Deactivated: u.Deleted})
				}
			}
		}
		lookups := words
		if len(lookups) > agentMaxMentionLookups {
			lookups = lookups[:agentMaxMentionLookups]
		}
		for _, w := range lookups {
			if strings.EqualFold(w, s.client.UserName()) {
				addUser(agentUser{ID: s.client.UserID(), Name: s.client.UserName()})
				continue
			}
			found, err := s.edge.UsersSearch(ctx, w, "", nil)
			if err != nil {
				return "", nil, err
			}
			for _, eu := range found {
				u := agentUserFromEdge(eu)
				s.rememberUser(u)
				addUser(u)
			}
		}
		box.SetUsers(users)
		box.SetUserGroups(s.loadUserGroups(ctx))
	}
	if strings.Contains(text, "#") {
		joined, err := s.joinedConversations(ctx)
		if err != nil {
			return "", nil, err
		}
		var chans []channelpicker.Channel
		for _, ch := range joined {
			if !ch.IsIM && !ch.IsMpIM && ch.Name != "" {
				chans = append(chans, channelpicker.Channel{ID: ch.ID, Name: ch.Name, Type: agentChannelType(ch)})
			}
		}
		box.SetChannels(chans)
	}
	wire = box.TranslateMentionsForSend(text)
	return wire, atWords(wire), nil
}

// loadUserGroups is usergroups.list's handles, fetched once per run. A
// failure leaves group mentions as plain text (and in unresolved).
func (s *agentSession) loadUserGroups(ctx context.Context) map[string]string {
	if s.userGroups != nil {
		return s.userGroups
	}
	var groups []slack.UserGroup
	_ = slackclient.RetryRateLimited(ctx, func() error {
		var err error
		groups, err = s.client.GetUserGroups(ctx)
		return err
	})
	s.userGroups = usergroupHandles(groups)
	return s.userGroups
}

// agentWriteResult is what every write command prints. A dry run has
// DryRun set, no TS for a new message, and the blocks it would send.
type agentWriteResult struct {
	OK                 bool           `json:"ok"`
	DryRun             bool           `json:"dry_run,omitempty"`
	Action             string         `json:"action"`
	Method             string         `json:"method"`
	ChannelID          string         `json:"channel_id"`
	TS                 string         `json:"ts,omitempty"`
	ThreadTS           string         `json:"thread_ts,omitempty"`
	Broadcast          bool           `json:"broadcast,omitempty"`
	Text               string         `json:"text,omitempty"`
	Blocks             []slack.Block  `json:"blocks,omitempty"`
	Emoji              string         `json:"emoji,omitempty"`
	Permalink          string         `json:"permalink,omitempty"`
	UnresolvedMentions []string       `json:"unresolved_mentions,omitempty"`
	Links              []reflink.Link `json:"links,omitempty"`
}

// composedText is message text ready for slackclient.ConvertMessage.
type composedText struct {
	wire       string
	unresolved []string
	links      []reflink.Link
}

// composeText translates text's mentions and, when linkify is set,
// links its bare references by the [agent] config. Linkify runs second
// so the mention tokens translation makes are spans it leaves alone.
func (s *agentSession) composeText(ctx context.Context, text string, linkify bool) (composedText, error) {
	wire, unresolved, err := s.translateMentions(ctx, text)
	if err != nil {
		return composedText{}, err
	}
	out := composedText{wire: wire, unresolved: unresolved}
	if linkify {
		a := s.cfg.Agent
		out.wire, out.links = reflink.Linkify(wire, reflink.Rules{
			GitHubOrg: a.GitHubOrg, LinearWorkspace: a.LinearWorkspace, LinearTeams: a.LinearTeams,
		})
	}
	return out, nil
}

// postAgentMessage sends text to channelID (in threadTS's thread when
// set) and reports it; dryRun stops before chat.postMessage.
func (s *agentSession) postAgentMessage(ctx context.Context, action, channelID, threadTS, text string, broadcast, linkify, dryRun bool) (agentWriteResult, error) {
	ct, err := s.composeText(ctx, text, linkify)
	if err != nil {
		return agentWriteResult{}, err
	}
	wire := ct.wire
	mr, block, err := slackclient.ConvertMessage(wire)
	if err != nil {
		return agentWriteResult{}, err
	}
	res := agentWriteResult{
		OK: true, Action: action, Method: "chat.postMessage", ChannelID: channelID,
		ThreadTS: threadTS, Broadcast: broadcast, Text: mr, UnresolvedMentions: ct.unresolved, Links: ct.links,
	}
	if dryRun {
		res.DryRun = true
		res.Blocks = []slack.Block{block}
		return res, nil
	}
	// A 429 means Slack posted nothing, so retrying cannot post twice;
	// any other error is returned as is, never retried.
	err = slackclient.RetryRateLimited(ctx, func() error {
		var err error
		if threadTS != "" {
			res.TS, res.Text, err = s.client.SendReply(ctx, channelID, threadTS, wire, broadcast)
		} else {
			res.TS, res.Text, err = s.client.SendMessage(ctx, channelID, wire)
		}
		return err
	})
	if err != nil {
		return agentWriteResult{}, err
	}
	res.Permalink = s.permalink(channelID, res.TS, threadTS)
	return res, nil
}

func runAgentSend(env *agentEnv, args []string) (any, error) {
	fs, workspace := newAgentFlags(env, "send")
	thread := fs.String("thread", "", "post as a reply in the thread whose parent has this ts")
	broadcast := fs.Bool("broadcast", false, "with --thread, also post the reply to the channel")
	noLinkify := fs.Bool("no-linkify", false, "leave repo#N and TEAM-N references as plain text")
	dryRun := fs.Bool("dry-run", false, "print what would be sent and send nothing")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 2 {
		return nil, agentUsageError("send takes a channel and the text (or - to read it from stdin); quote the text")
	}
	if *thread != "" && !agentTSRe.MatchString(*thread) {
		return nil, agentUsageError("--thread %q is not a Slack ts", *thread)
	}
	if *broadcast && *thread == "" {
		return nil, agentUsageError("--broadcast needs --thread")
	}
	text, err := readAgentText(env, pos[1])
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
	return s.postAgentMessage(env.ctx, "send", ch.ID, *thread, text, *broadcast, !*noLinkify, *dryRun)
}

// openForPermalink parses a permalink argument and connects to its
// workspace, refusing one from another workspace (checkPermalinkWorkspace).
func openForPermalink(env *agentEnv, workspace, arg string) (*agentSession, string, string, string, error) {
	pl, ok := slackurl.Parse(arg)
	if !ok {
		return nil, "", "", "", agentUsageError("%q is not a Slack message permalink", arg)
	}
	s, err := env.open(env.ctx, workspace, pl.Subdomain)
	if err != nil {
		return nil, "", "", "", err
	}
	if err := s.checkPermalinkWorkspace(pl.Subdomain); err != nil {
		return nil, "", "", "", err
	}
	return s, string(pl.ChannelID), string(pl.MessageTS), string(pl.ThreadTS), nil
}

func runAgentReply(env *agentEnv, args []string) (any, error) {
	fs, workspace := newAgentFlags(env, "reply")
	broadcast := fs.Bool("broadcast", false, "also post the reply to the channel")
	noLinkify := fs.Bool("no-linkify", false, "leave repo#N and TEAM-N references as plain text")
	dryRun := fs.Bool("dry-run", false, "print what would be sent and send nothing")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 2 {
		return nil, agentUsageError("reply takes a permalink and the text (or - to read it from stdin); quote the text")
	}
	text, err := readAgentText(env, pos[1])
	if err != nil {
		return nil, err
	}
	s, channelID, ts, threadTS, err := openForPermalink(env, *workspace, pos[0])
	if err != nil {
		return nil, err
	}
	// A reply's permalink names its thread; a parent's is its own.
	root := threadTS
	if root == "" {
		root = ts
	}
	return s.postAgentMessage(env.ctx, "reply", channelID, root, text, *broadcast, !*noLinkify, *dryRun)
}

func runAgentReact(env *agentEnv, args []string) (any, error) {
	fs, workspace := newAgentFlags(env, "react")
	remove := fs.Bool("remove", false, "remove your reaction instead of adding it")
	dryRun := fs.Bool("dry-run", false, "print what would be done and do nothing")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 2 {
		return nil, agentUsageError("react takes a permalink and an emoji name")
	}
	emoji := strings.Trim(strings.TrimSpace(pos[1]), ":")
	if emoji == "" || strings.ContainsAny(emoji, " :") {
		return nil, agentUsageError("%q is not an emoji name (like thumbsup or :tada:)", pos[1])
	}
	s, channelID, ts, threadTS, err := openForPermalink(env, *workspace, pos[0])
	if err != nil {
		return nil, err
	}
	res := agentWriteResult{OK: true, Action: "react", Method: "reactions.add", ChannelID: channelID, TS: ts, ThreadTS: threadTS, Emoji: emoji,
		Permalink: s.permalink(channelID, ts, threadTS)}
	if *remove {
		res.Action, res.Method = "unreact", "reactions.remove"
	}
	if *dryRun {
		res.DryRun = true
		return res, nil
	}
	err = slackclient.RetryRateLimited(env.ctx, func() error {
		if *remove {
			return s.client.RemoveReaction(env.ctx, channelID, ts, emoji)
		}
		return s.client.AddReaction(env.ctx, channelID, ts, emoji)
	})
	// Already in the state asked for: the command did what it says.
	if code := slackErrorCode(err); code == "already_reacted" || code == "no_reaction" {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

func runAgentEdit(env *agentEnv, args []string) (any, error) {
	fs, workspace := newAgentFlags(env, "edit")
	noLinkify := fs.Bool("no-linkify", false, "leave repo#N and TEAM-N references as plain text")
	dryRun := fs.Bool("dry-run", false, "print what would be sent and change nothing")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 2 {
		return nil, agentUsageError("edit takes a permalink and the new text (or - to read it from stdin); quote the text")
	}
	text, err := readAgentText(env, pos[1])
	if err != nil {
		return nil, err
	}
	s, channelID, ts, threadTS, err := openForPermalink(env, *workspace, pos[0])
	if err != nil {
		return nil, err
	}
	if err := s.requireOwnMessage(env.ctx, channelID, ts, threadTS); err != nil {
		return nil, err
	}
	ct, err := s.composeText(env.ctx, text, !*noLinkify)
	if err != nil {
		return nil, err
	}
	wire := ct.wire
	mr, block, err := slackclient.ConvertMessage(wire)
	if err != nil {
		return nil, err
	}
	res := agentWriteResult{OK: true, Action: "edit", Method: "chat.update", ChannelID: channelID, TS: ts, ThreadTS: threadTS,
		Text: mr, Permalink: s.permalink(channelID, ts, threadTS), UnresolvedMentions: ct.unresolved, Links: ct.links}
	if *dryRun {
		res.DryRun = true
		res.Blocks = []slack.Block{block}
		return res, nil
	}
	err = slackclient.RetryRateLimited(env.ctx, func() error {
		var err error
		res.Text, err = s.client.EditMessage(env.ctx, channelID, ts, wire)
		return err
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func runAgentDelete(env *agentEnv, args []string) (any, error) {
	fs, workspace := newAgentFlags(env, "delete")
	dryRun := fs.Bool("dry-run", false, "check the message is yours and delete nothing")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 1 {
		return nil, agentUsageError("delete takes one permalink")
	}
	s, channelID, ts, threadTS, err := openForPermalink(env, *workspace, pos[0])
	if err != nil {
		return nil, err
	}
	if err := s.requireOwnMessage(env.ctx, channelID, ts, threadTS); err != nil {
		return nil, err
	}
	res := agentWriteResult{OK: true, Action: "delete", Method: "chat.delete", ChannelID: channelID, TS: ts, ThreadTS: threadTS}
	if *dryRun {
		res.DryRun = true
		res.Permalink = s.permalink(channelID, ts, threadTS)
		return res, nil
	}
	err = slackclient.RetryRateLimited(env.ctx, func() error {
		return s.client.RemoveMessage(env.ctx, channelID, ts)
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// requireOwnMessage fetches the message at ts and refuses unless the
// signed-in user wrote it. Slack lets an admin edit or delete other
// people's messages; these commands are for one's own, whatever the
// account's role.
func (s *agentSession) requireOwnMessage(ctx context.Context, channelID, ts, threadTS string) error {
	root := threadTS
	if root == "" {
		root = ts
	}
	msgs, err := s.client.GetRepliesBetween(ctx, channelID, root, ts, ts)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if m.Timestamp != ts {
			continue
		}
		if m.User != s.client.UserID() {
			author := m.User
			if author == "" {
				author = m.BotID
			}
			return agentRefused("message %s in %s is by %s, not you; slk only edits and deletes your own messages", ts, channelID, author)
		}
		return nil
	}
	return agentNotFound("no message %s in %s", ts, channelID)
}
