package slackclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

func TestConvertMessage_MatchesWhatEditSends(t *testing.T) {
	var sent url.Values
	mock := &mockSlackAPI{
		updateMessageFn: func(channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error) {
			_, vals, err := slack.UnsafeApplyMsgOptions("tok", channelID, "https://slack.com/api/", options...)
			if err != nil {
				t.Fatal(err)
			}
			sent = vals
			return channelID, timestamp, "", nil
		},
	}
	c := &Client{api: mock}

	mr, err := c.EditMessage(context.Background(), "C1", "1.000001", "*bold* and <@U1>")
	if err != nil {
		t.Fatal(err)
	}
	gotMr, block, err := ConvertMessage("*bold* and <@U1>")
	if err != nil {
		t.Fatal(err)
	}
	if gotMr != mr {
		t.Errorf("ConvertMessage text = %q, EditMessage sent %q", gotMr, mr)
	}
	if block == nil || len(block.Elements) == 0 {
		t.Fatalf("block = %#v, want a rich_text block", block)
	}
	if sent.Get("text") != mr {
		t.Errorf("chat.update text = %q, want %q", sent.Get("text"), mr)
	}
}

func TestSearchMessagesPage_PassesPageAndSort(t *testing.T) {
	var got slack.SearchParameters
	mock := &mockSlackAPI{
		searchMessagesFn: func(ctx context.Context, query string, params slack.SearchParameters) (*slack.SearchMessages, error) {
			got = params
			return &slack.SearchMessages{}, nil
		},
	}
	c := &Client{api: mock}
	if _, err := c.SearchMessagesPage(context.Background(), "q", SearchOptions{Count: 5, Page: 3, Sort: "timestamp", SortDir: "asc"}); err != nil {
		t.Fatal(err)
	}
	if got.Count != 5 || got.Page != 3 || got.Sort != "timestamp" || got.SortDirection != "asc" {
		t.Errorf("params = %+v, want count 5, page 3, timestamp asc", got)
	}
}

func TestSearchMessagesPage_ZeroOptionsKeepDefaults(t *testing.T) {
	var got slack.SearchParameters
	mock := &mockSlackAPI{
		searchMessagesFn: func(ctx context.Context, query string, params slack.SearchParameters) (*slack.SearchMessages, error) {
			got = params
			return &slack.SearchMessages{}, nil
		},
	}
	c := &Client{api: mock}
	if _, err := c.SearchMessagesPage(context.Background(), "q", SearchOptions{}); err != nil {
		t.Fatal(err)
	}
	if want := slack.NewSearchParameters(); got != want {
		t.Errorf("params = %+v, want slack-go defaults %+v", got, want)
	}
}

func TestTeamURL_FromAuthTest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"url":"https://acme.slack.com/","team":"Acme","user":"grant","team_id":"T1","user_id":"U1"}`))
	}))
	defer srv.Close()
	c := NewClient("xoxc-test", "d-cookie")
	pointClientAtTestServer(t, c, srv)
	c.api = slack.New("xoxc-test", slack.OptionHTTPClient(c.httpClient), slack.OptionAPIURL(c.apiBaseURL))
	if got := c.TeamURL(); got != "" {
		t.Errorf("TeamURL before Connect = %q, want empty", got)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.TeamURL(); got != "https://acme.slack.com/" {
		t.Errorf("TeamURL = %q, want auth.test's url", got)
	}
}

func TestWaitOutRateLimit_PostFormRateLimit(t *testing.T) {
	err := fmt.Errorf("activity: %w", &rateLimitError{method: "activity.feed", retryAfter: time.Millisecond})
	limited, waitErr := WaitOutRateLimit(context.Background(), err)
	if !limited || waitErr != nil {
		t.Errorf("limited = %v, err = %v; want true, nil", limited, waitErr)
	}
}

func TestWaitOutRateLimit_CancelledDuringWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	limited, err := WaitOutRateLimit(ctx, &slack.RateLimitedError{RetryAfter: time.Hour})
	if !limited || !errors.Is(err, context.Canceled) {
		t.Errorf("limited = %v, err = %v; want true, context.Canceled", limited, err)
	}
}

func TestRetryRateLimited(t *testing.T) {
	calls := 0
	err := RetryRateLimited(context.Background(), func() error {
		calls++
		if calls < 3 {
			return &slack.RateLimitedError{RetryAfter: time.Millisecond}
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Errorf("err = %v, calls = %d; want nil, 3", err, calls)
	}

	boom := errors.New("boom")
	calls = 0
	err = RetryRateLimited(context.Background(), func() error {
		calls++
		return boom
	})
	if !errors.Is(err, boom) || calls != 1 {
		t.Errorf("err = %v, calls = %d; want boom after one call", err, calls)
	}
}
