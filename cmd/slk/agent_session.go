// This file connects an agent command to Slack: it picks the stored
// workspace token the way `slk export` does, refreshes it from the
// desktop app, and connects one client. Grid org-session routing is the
// TUI's own (orgOrOwnClient), with the sibling sessions connected only
// when a command needs one. Nothing here touches the SQLite cache.

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gammons/slk/internal/config"
	slackclient "github.com/gammons/slk/internal/slack"
	"github.com/gammons/slk/internal/slack/edge"
	"github.com/gammons/slk/internal/slackdesktop"
	"github.com/slack-go/slack"
)

// agentSession is one connected workspace plus what a single command
// run learns along the way (joined conversations, resolved users),
// so a command asks Slack for each at most once.
type agentSession struct {
	client *slackclient.Client
	edge   *edge.Client
	cfg    config.Config
	// explicit is true when the user named the workspace, which makes
	// a permalink for another workspace their call, not a mistake.
	explicit bool

	router *workspaceRouter
	wctx   *WorkspaceContext
	// siblings connects every other stored workspace, for Grid
	// org-session routing. Called at most once, and only on a Grid
	// member session (see orgClient).
	siblings     func(ctx context.Context) []*slackclient.Client
	siblingsDone bool

	joined       []slack.Channel
	joinedLoaded bool
	users        map[string]agentUser
	userGroups   map[string]string
}

// newAgentSession wraps a connected client.
func newAgentSession(client *slackclient.Client, accessToken string, cfg config.Config, explicit bool) *agentSession {
	wctx := &WorkspaceContext{
		Client:     client,
		TeamID:     client.TeamID(),
		TeamName:   client.TeamName(),
		UserID:     client.UserID(),
		UserHandle: client.UserName(),
		UserNames:  newUserNameStore(nil),
	}
	router := newWorkspaceRouter()
	router.Add(wctx)
	router.Set(wctx)
	return &agentSession{
		client: client,
		// The BrowserTransport-carrying client, as connectWorkspace
		// builds it.
		edge:     edge.New(accessToken, client.TeamID(), client.HTTPClient()),
		cfg:      cfg,
		explicit: explicit,
		router:   router,
		wctx:     wctx,
		users:    map[string]agentUser{},
	}
}

// openAgentSession is agentEnv.open for the real program.
func openAgentSession(ctx context.Context, workspace, hint string) (*agentSession, error) {
	store := slackclient.NewTokenStore(filepath.Join(xdgData(), "tokens"))
	tokens, err := store.List()
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	cfg, _ := config.Load(filepath.Join(xdgConfig(), "config.toml")) // best-effort, as in exportChannel
	ordered := config.OrderTokens(tokens, cfg)
	tok, err := pickAgentToken(ordered, workspace, hint)
	if err != nil {
		return nil, err
	}
	connect := func(ctx context.Context, tok slackclient.Token) (*slackclient.Client, slackclient.Token, error) {
		// xoxc tokens expire; refresh from the desktop app as the TUI
		// and `slk export` do. remintTokens keeps the stored token on
		// any failure.
		tok = remintTokens(ctx, []slackclient.Token{tok}, slackdesktop.Cookie, slackdesktop.Tokens, slackclient.MintToken, store.Save)[0]
		client := slackclient.NewClient(tok.AccessToken, tok.Cookie)
		seedVersionTS(client.Envelope(), cfg, tok.TeamID)
		if err := client.Connect(ctx); err != nil {
			return nil, tok, fmt.Errorf("connecting to %s: %w", tok.TeamName, err)
		}
		return client, tok, nil
	}
	client, tok, err := connect(ctx, tok)
	if err != nil {
		return nil, err
	}
	s := newAgentSession(client, tok.AccessToken, cfg, workspace != "")
	s.siblings = func(ctx context.Context) []*slackclient.Client {
		var out []*slackclient.Client
		for _, ot := range ordered {
			if ot.Token.TeamID == tok.TeamID {
				continue
			}
			if c, _, err := connect(ctx, ot.Token); err == nil {
				out = append(out, c)
			}
		}
		return out
	}
	return s, nil
}

// pickAgentToken chooses the workspace: the one named, else the one a
// permalink's subdomain names, else the only one configured.
func pickAgentToken(tokens []config.OrderedToken, workspace, hint string) (slackclient.Token, error) {
	if workspace == "" && hint != "" {
		if tok, err := selectExportToken(tokens, hint); err == nil {
			return tok, nil
		}
	}
	return selectExportToken(tokens, workspace)
}

// orgClient is the client an org-served call runs on: the same user's
// org-level session on a Grid member workspace (orgOrOwnClient), else
// the workspace's own. The sibling workspaces are connected the first
// time a Grid member session asks, never off a grid.
func (s *agentSession) orgClient(ctx context.Context) *slackclient.Client {
	if !s.client.IsGridMember() {
		return s.client
	}
	if !s.siblingsDone && s.siblings != nil {
		s.siblingsDone = true
		for _, c := range s.siblings(ctx) {
			s.router.Add(&WorkspaceContext{Client: c, TeamID: c.TeamID(), UserID: c.UserID()})
		}
	}
	return orgOrOwnClient(s.router, s.wctx)
}

// joinedConversations is users.conversations for every type, fetched
// once per run (GetChannels waits out rate limits itself).
func (s *agentSession) joinedConversations(ctx context.Context) ([]slack.Channel, error) {
	if s.joinedLoaded {
		return s.joined, nil
	}
	chans, err := s.client.GetChannels(ctx)
	if err != nil {
		return nil, err
	}
	s.joined, s.joinedLoaded = chans, true
	for _, ch := range chans {
		s.wctx.MarkConversation(ch.ID)
	}
	return chans, nil
}

// checkPermalinkWorkspace refuses a permalink from another workspace
// unless the user named the workspace: acting on the wrong workspace's
// channel ID is at best channel_not_found. The session's own subdomain
// and its configured permalink_domains count, as in the TUI.
func (s *agentSession) checkPermalinkWorkspace(subdomain string) error {
	if s.explicit || subdomain == "" {
		return nil
	}
	if strings.EqualFold(subdomain, s.client.TeamSubdomain()) {
		return nil
	}
	for _, d := range s.cfg.PermalinkDomains(s.client.TeamID()) {
		if strings.EqualFold(d, subdomain) {
			return nil
		}
	}
	return agentRefused("the permalink is for %s.slack.com but workspace %s (%s.slack.com) is selected; pass --workspace to use it anyway",
		subdomain, s.client.TeamName(), s.client.TeamSubdomain())
}
