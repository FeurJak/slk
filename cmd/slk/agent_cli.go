// This file is the front of the agent commands (`slk search`, `slk
// send`, ...): the command table, flag parsing, and the JSON contract.
// Every command prints one JSON value on stdout, or one JSON error on
// stderr with a non-zero exit. None of them starts the TUI. See
// docs/agent-cli.md and docs/superpowers/specs/2026-11-27-agent-cli-design.md.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/slack-go/slack"
)

// agentEnv is what a command runs with. open connects the session; it
// is a field so tests hand commands a session against a fake Slack.
type agentEnv struct {
	ctx    context.Context
	stdin  io.Reader
	stderr io.Writer
	// open connects to workspace (a slug, team ID, name or domain; ""
	// when the user gave none). hint is a permalink's subdomain, used
	// to pick the workspace when none was named.
	open func(ctx context.Context, workspace, hint string) (*agentSession, error)
}

// agentCommand is one subcommand: its usage line, a one-line summary
// for `slk --help`, and its body. run returns the value to print.
type agentCommand struct {
	synopsis string
	summary  string
	run      func(env *agentEnv, args []string) (any, error)
}

// agentCommandTable is every agent command, by name.
func agentCommandTable() map[string]agentCommand {
	return map[string]agentCommand{
		"search":   {"search <query> [--count N] [--page N] [--sort score|timestamp] [--sort-dir desc|asc]", "Search messages (Slack modifiers like in:#chan from:@user pass through)", runAgentSearch},
		"channels": {"channels [--type public,private,mpim,im] [--member-count]", "List the conversations you are in", runAgentChannels},
		"users":    {"users (--query <text> | --id U1,U2)", "Look users up by name or ID", runAgentUsers},
		"history":  {"history <channel> [--since T] [--until T] [--limit N] [--replies]", "Read a conversation's messages, oldest first", runAgentHistory},
		"thread":   {"thread (<permalink> | <channel> <ts>)", "Read a thread: the parent and every reply", runAgentThread},
		"mentions": {"mentions [--since T] [--limit N]", "Messages that mention you, your usergroups, or @channel/@here", runAgentMentions},
		"send":     {"send <channel> (<text> | -) [--thread TS] [--broadcast] [--dry-run]", "Post a message (markdown, @name and #channel translated)", runAgentSend},
		"reply":    {"reply <permalink> (<text> | -) [--broadcast] [--dry-run]", "Reply in the thread of a message", runAgentReply},
		"react":    {"react <permalink> <emoji> [--remove] [--dry-run]", "Add (or remove) a reaction", runAgentReact},
		"edit":     {"edit <permalink> (<text> | -) [--dry-run]", "Edit one of your own messages", runAgentEdit},
		"delete":   {"delete <permalink> [--dry-run]", "Delete one of your own messages", runAgentDelete},
	}
}

// isAgentCommand reports whether name is an agent command.
func isAgentCommand(name string) bool {
	_, ok := agentCommandTable()[name]
	return ok
}

// agentHelpText is the agent section of `slk --help`.
func agentHelpText() string {
	table := agentCommandTable()
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return agentHelpOrder(names[i]) < agentHelpOrder(names[j]) })
	var b strings.Builder
	b.WriteString("Agent commands (JSON on stdout; see 'slk <command> --help' and docs/agent-cli.md):\n")
	for _, name := range names {
		fmt.Fprintf(&b, "  slk %-10s %s\n", name, table[name].summary)
	}
	b.WriteString("  Common: --workspace <slug|team id|name>; errors are JSON on stderr, exit 1 (2 for usage)\n")
	return b.String()
}

// agentHelpOrder lists reads before writes in the help.
func agentHelpOrder(name string) int {
	for i, n := range []string{"search", "channels", "users", "history", "thread", "mentions", "send", "reply", "react", "edit", "delete"} {
		if n == name {
			return i
		}
	}
	return 100
}

// runAgent runs an agent command as main does: on the real stdio, with
// Ctrl-C cancelling the context. Returns the exit code.
func runAgent(name string, args []string) int {
	// Shared code logs with the stdlib logger; on the agent path that
	// must not land in the JSON streams. SLK_DEBUG sends it to stderr.
	// debuglog.Init is deliberately not called: it truncates
	// slk-debug.log in the cwd, which may be a running TUI's log.
	if os.Getenv("SLK_DEBUG") == "" {
		log.SetOutput(io.Discard)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	context.AfterFunc(ctx, stop)
	env := &agentEnv{ctx: ctx, stdin: os.Stdin, stderr: os.Stderr, open: openAgentSession}
	return runAgentWith(env, name, args, os.Stdout)
}

// runAgentWith runs command name and writes its result to stdout or
// its error to env.stderr. Exit codes: 0 ok, 1 failure, 2 usage.
func runAgentWith(env *agentEnv, name string, args []string, stdout io.Writer) int {
	cmd, ok := agentCommandTable()[name]
	if !ok {
		writeAgentError(env.stderr, agentUsageError("unknown command %q", name))
		return 2
	}
	result, err := cmd.run(env, args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		writeAgentError(env.stderr, err)
		var ue *agentError
		if errors.As(err, &ue) && ue.Kind == "usage" {
			return 2
		}
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false) // keep <@U1> and & readable
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		writeAgentError(env.stderr, err)
		return 1
	}
	return 0
}

// agentError is an error with a kind an agent can branch on: "usage"
// (bad arguments), "not_found", "refused" (slk will not do it), or
// "slack" (Slack refused; SlackError carries its code).
type agentError struct {
	Kind    string
	Message string
}

func (e *agentError) Error() string { return e.Message }

func agentUsageError(format string, args ...any) error {
	return &agentError{Kind: "usage", Message: fmt.Sprintf(format, args...)}
}

func agentNotFound(format string, args ...any) error {
	return &agentError{Kind: "not_found", Message: fmt.Sprintf(format, args...)}
}

func agentRefused(format string, args ...any) error {
	return &agentError{Kind: "refused", Message: fmt.Sprintf(format, args...)}
}

// agentErrorBody is the JSON written to stderr under "error".
type agentErrorBody struct {
	Kind       string `json:"kind"`
	Message    string `json:"message"`
	SlackError string `json:"slack_error,omitempty"`
}

func writeAgentError(w io.Writer, err error) {
	body := agentErrorBody{Kind: "error", Message: err.Error()}
	var ae *agentError
	var se slack.SlackErrorResponse
	switch {
	case errors.As(err, &ae):
		body.Kind = ae.Kind
	case errors.Is(err, context.Canceled):
		body.Kind = "cancelled"
	case errors.As(err, &se):
		body.Kind = "slack"
		body.SlackError = se.Err
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(struct {
		Error agentErrorBody `json:"error"`
	}{body})
}

// slackErrorCode is the code of a Slack refusal in err's chain, "" if
// err is not one.
func slackErrorCode(err error) string {
	var se slack.SlackErrorResponse
	if errors.As(err, &se) {
		return se.Err
	}
	return ""
}

// newAgentFlags is a command's flag set with the --workspace flag every
// command takes. -h prints the usage line and the flags on stderr.
func newAgentFlags(env *agentEnv, name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("slk "+name, flag.ContinueOnError)
	fs.SetOutput(env.stderr)
	workspace := fs.String("workspace", "", "workspace slug, team ID, name or domain (required when more than one is configured)")
	fs.Usage = func() {
		fmt.Fprintf(env.stderr, "Usage: slk %s\n\n%s\n\nFlags:\n", agentCommandTable()[name].synopsis, agentCommandTable()[name].summary)
		fs.PrintDefaults()
	}
	return fs, workspace
}

// parseInterspersed parses args with fs, allowing flags after
// positional arguments (`slk send C1 hi --dry-run`), which the flag
// package alone stops at. "--" ends flag parsing; "-" is positional.
// It returns the positionals in order. A flag error is a usage error;
// -h is flag.ErrHelp.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, agentUsageError("%v", err)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positionals, nil
		}
		// fs.Parse consumed a "--" if it stopped there; everything
		// after it is positional.
		if len(args) > len(rest) && args[len(args)-len(rest)-1] == "--" {
			return append(positionals, rest...), nil
		}
		positionals = append(positionals, rest[0])
		args = rest[1:]
	}
}

// readAgentText returns arg, or all of stdin when arg is "-" (one
// trailing newline dropped, as a shell heredoc adds one). Empty text is
// a usage error: Slack refuses it anyway, and an agent that piped
// nothing almost certainly meant something.
func readAgentText(env *agentEnv, arg string) (string, error) {
	text := arg
	if arg == "-" {
		raw, err := io.ReadAll(env.stdin)
		if err != nil {
			return "", fmt.Errorf("reading stdin: %w", err)
		}
		text = strings.TrimSuffix(string(raw), "\n")
	}
	if strings.TrimSpace(text) == "" {
		return "", agentUsageError("message text is empty")
	}
	return text, nil
}
