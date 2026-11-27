package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestParseInterspersed_FlagsAfterPositionals(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	thread := fs.String("thread", "", "")
	dry := fs.Bool("dry-run", false, "")
	args, err := parseInterspersed(fs, []string{"C1", "--thread", "1.2", "-", "--dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	if *thread != "1.2" || !*dry {
		t.Errorf("thread %q dry %v; want 1.2 true", *thread, *dry)
	}
	if strings.Join(args, "|") != "C1|-" {
		t.Errorf("positionals = %q, want C1 and -", args)
	}
}

func TestParseInterspersed_DoubleDashEndsFlags(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "")
	args, err := parseInterspersed(fs, []string{"C1", "--", "--dry-run", "-x"})
	if err != nil {
		t.Fatal(err)
	}
	if *dry {
		t.Error("--dry-run after -- was parsed as a flag")
	}
	if strings.Join(args, "|") != "C1|--dry-run|-x" {
		t.Errorf("positionals = %q", args)
	}
}

// runAgentForTest runs one command against a session the test built.
func runAgentForTest(t *testing.T, s *agentSession, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	env := &agentEnv{
		ctx:    context.Background(),
		stdin:  strings.NewReader(stdin),
		stderr: &errOut,
		open: func(context.Context, string, string) (*agentSession, error) {
			if s == nil {
				t.Fatal("command opened a session; this test expects it not to")
			}
			return s, nil
		},
	}
	code = runAgentWith(env, args[0], args[1:], &out)
	return out.String(), errOut.String(), code
}

func decodeAgentError(t *testing.T, stderr string) agentErrorBody {
	t.Helper()
	var body struct {
		Error agentErrorBody `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &body); err != nil {
		t.Fatalf("stderr is not one JSON error: %v\n%s", err, stderr)
	}
	return body.Error
}

func TestAgent_UsageErrorIsJSONExit2(t *testing.T) {
	stdout, stderr, code := runAgentForTest(t, nil, "", "send", "C1")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	if e := decodeAgentError(t, stderr); e.Kind != "usage" || e.Message == "" {
		t.Errorf("error = %+v, want kind usage with a message", e)
	}
}

func TestAgent_FailureIsJSONExit1(t *testing.T) {
	var errOut, out bytes.Buffer
	env := &agentEnv{
		ctx:    context.Background(),
		stdin:  strings.NewReader(""),
		stderr: &errOut,
		open: func(context.Context, string, string) (*agentSession, error) {
			return nil, errors.New("no workspaces configured")
		},
	}
	code := runAgentWith(env, "channels", nil, &out)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if e := decodeAgentError(t, errOut.String()); e.Kind != "error" || !strings.Contains(e.Message, "no workspaces") {
		t.Errorf("error = %+v", e)
	}
}

func TestAgent_HelpExitsZeroWithoutASession(t *testing.T) {
	for name := range agentCommandTable() {
		_, stderr, code := runAgentForTest(t, nil, "", name, "--help")
		if code != 0 {
			t.Errorf("%s --help: exit %d", name, code)
		}
		if !strings.Contains(stderr, "slk "+name) {
			t.Errorf("%s --help printed %q, want its usage", name, stderr)
		}
	}
}

func TestAgentCommandsAreListedInHelp(t *testing.T) {
	help := agentHelpText()
	for name := range agentCommandTable() {
		if !strings.Contains(help, "slk "+name) {
			t.Errorf("agentHelpText does not mention %q", name)
		}
	}
}

// TestAgentFilesNeverTouchTheCache pins the design's promise that the
// agent commands never write slk's SQLite cache: they do not import
// it, so they cannot open it (cache.New migrates, which writes).
func TestAgentFilesNeverTouchTheCache(t *testing.T) {
	files, err := filepath.Glob("agent_*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no agent files found: %v", err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(fset, f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if path == "github.com/gammons/slk/internal/cache" {
				t.Errorf("%s imports internal/cache; agent commands must not open the cache", f)
			}
		}
	}
}
