package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/edgewatch"
	"github.com/reorx/hookploy/internal/model"
)

// telegramHarness stands up a fake Bot API and a provider pointed at it.
// Requests land on the server's goroutine while the test reads from its own,
// so everything shared goes through the mutex.
type telegramHarness struct {
	provider *telegramProvider

	mu       sync.Mutex
	gotPaths []string
	gotMsgs  []sendMessage
	status   int
	body     string
}

func newTelegramHarness(t *testing.T) *telegramHarness {
	t.Helper()
	th := &telegramHarness{status: http.StatusOK, body: `{"ok":true}`}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var msg sendMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Errorf("request body is not a sendMessage: %v (%s)", err, raw)
		}
		th.mu.Lock()
		th.gotPaths = append(th.gotPaths, r.URL.Path)
		th.gotMsgs = append(th.gotMsgs, msg)
		status, body := th.status, th.body
		th.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	th.provider = &telegramProvider{
		creds:   config.Telegram{BotToken: "123:AA", ChatID: "-100777"},
		http:    ts.Client(),
		apiBase: ts.URL,
	}
	return th
}

// paths and msgs snapshot what the fake Bot API has received so far.
func (th *telegramHarness) paths() []string {
	th.mu.Lock()
	defer th.mu.Unlock()
	return append([]string(nil), th.gotPaths...)
}

func (th *telegramHarness) msgs() []sendMessage {
	th.mu.Lock()
	defer th.mu.Unlock()
	return append([]sendMessage(nil), th.gotMsgs...)
}

// reject makes the next responses fail with a Telegram-style error body.
func (th *telegramHarness) reject(status int, body string) {
	th.mu.Lock()
	defer th.mu.Unlock()
	th.status, th.body = status, body
}

func failedEvent() Event {
	now := time.Now()
	return Event{
		Kind:      model.EventDeployFailed,
		CreatedAt: now,
		Deploy: &DeployEvent{
			Service:  "web",
			DeployID: "dp_abc",
			Status:   model.StatusFailed,
			Instances: []InstanceResult{
				{Instance: "a", Server: "s1", Status: model.StatusFailed, Error: "op 2 (compose.up): exit 1"},
			},
			FinishedAt: now.Add(42 * time.Second),
		},
	}
}

// Behavior: a send hits the Bot API's sendMessage endpoint for the configured
// bot, addressed to the configured chat.
func TestTelegramSendPostsToTheBotEndpoint(t *testing.T) {
	th := newTelegramHarness(t)
	if err := th.provider.Send(context.Background(), failedEvent()); err != nil {
		t.Fatal(err)
	}
	paths := th.paths()
	if len(paths) != 1 || paths[0] != "/bot123:AA/sendMessage" {
		t.Fatalf("requested %v, want one POST to /bot123:AA/sendMessage", paths)
	}
	msg := th.msgs()[0]
	if msg.ChatID != "-100777" {
		t.Errorf("chat_id = %q, want the configured one", msg.ChatID)
	}
	if msg.ParseMode != "HTML" {
		t.Errorf("parse_mode = %q, want HTML", msg.ParseMode)
	}
	if !strings.Contains(msg.Text, "web") || !strings.Contains(msg.Text, "compose.up") {
		t.Errorf("message should name the service and the failing op:\n%s", msg.Text)
	}
}

// Behavior: a rejected send is an error, and the error carries Telegram's own
// explanation — the difference between a fixable log line and a mystery.
func TestTelegramRejectionIsAnErrorCarryingTheReason(t *testing.T) {
	th := newTelegramHarness(t)
	th.reject(http.StatusBadRequest, `{"ok":false,"description":"chat not found"}`)
	err := th.provider.Send(context.Background(), failedEvent())
	if err == nil {
		t.Fatal("a non-200 response must be an error so the Hub retries")
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("error should quote Telegram's description, got %v", err)
	}
}

// Behavior: everything interpolated into the message is escaped. Error text
// is arbitrary command output, and an unescaped angle bracket would break
// HTML parse mode and get the whole message rejected.
func TestTelegramEscapesInterpolatedText(t *testing.T) {
	th := newTelegramHarness(t)
	ev := failedEvent()
	ev.Deploy.Instances[0].Error = `exec: <script> & "quoted"`
	ev.Deploy.Service = "a<b"
	if err := th.provider.Send(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	text := th.msgs()[0].Text
	if strings.Contains(text, "<script>") || strings.Contains(text, "a<b") {
		t.Errorf("interpolated text was not escaped:\n%s", text)
	}
	if !strings.Contains(text, "&lt;script&gt;") {
		t.Errorf("escaped form missing:\n%s", text)
	}
	if !strings.Contains(text, "<b>") {
		t.Errorf("the formatting tags this package emits must survive:\n%s", text)
	}
}

// Behavior: the deploy link appears only when there is one to show.
func TestTelegramIncludesTheDeployLinkWhenSet(t *testing.T) {
	th := newTelegramHarness(t)
	if err := th.provider.Send(context.Background(), failedEvent()); err != nil {
		t.Fatal(err)
	}
	if first := th.msgs()[0].Text; strings.Contains(first, "http") {
		t.Errorf("no link was set, none should appear:\n%s", first)
	}

	ev := failedEvent()
	ev.Deploy.URL = "https://deploy.example.com/ui/deploys/dp_abc"
	if err := th.provider.Send(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if second := th.msgs()[1].Text; !strings.Contains(second, ev.Deploy.URL) {
		t.Errorf("link missing from the message:\n%s", second)
	}
}

// Behavior: a build-failure event renders its deploy-level error, since it
// has no instances to list.
func TestTelegramRendersDeployLevelErrors(t *testing.T) {
	th := newTelegramHarness(t)
	ev := Event{
		Kind: model.EventDeployFailed,
		Deploy: &DeployEvent{
			Service: "web", DeployID: "dp_x",
			Status: model.StatusFailed, Error: "payload.digest is not a sha256",
		},
	}
	if err := th.provider.Send(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if text := th.msgs()[0].Text; !strings.Contains(text, "payload.digest is not a sha256") {
		t.Errorf("deploy-level error missing:\n%s", text)
	}
}

// Behavior: main.started renders as the version and nothing else — the
// deploy body has no service, no instances and no timing to show.
func TestTelegramRendersMainStarted(t *testing.T) {
	th := newTelegramHarness(t)
	ev := Event{
		Kind: model.EventMainStarted,
		Node: &NodeEvent{Type: NodeMain, Name: "main", ReleaseVersion: "v0.4.1"},
	}
	if err := th.provider.Send(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	text := th.msgs()[0].Text
	if !strings.Contains(text, "main started") || !strings.Contains(text, "v0.4.1") {
		t.Errorf("startup message should headline the event and name the version:\n%s", text)
	}
	t.Logf("rendered message:\n%s", text)
}

// Behavior: an outage message leads with the server, then says how long it
// has been gone and what it was last running.
func TestTelegramRendersEdgeOffline(t *testing.T) {
	th := newTelegramHarness(t)
	ev := Event{
		Kind: model.EventEdgeOffline,
		Node: &NodeEvent{
			Type: NodeEdge, Name: "ali-hk-01", ReleaseVersion: "v0.4.1",
			DownDuration: 5*time.Minute + 12*time.Second,
		},
	}
	if err := th.provider.Send(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	text := th.msgs()[0].Text
	for _, want := range []string{"edge offline", "ali-hk-01", "5m12s", "v0.4.1"} {
		if !strings.Contains(text, want) {
			t.Errorf("outage message is missing %q:\n%s", want, text)
		}
	}
	t.Logf("rendered message:\n%s", text)
}

// Behavior: the all-clear reports the whole outage and the version the edge
// came back on — a rolling edge upgrade is the common reason for one.
func TestTelegramRendersEdgeOnline(t *testing.T) {
	th := newTelegramHarness(t)
	ev := Event{
		Kind: model.EventEdgeOnline,
		Node: &NodeEvent{
			Type: NodeEdge, Name: "ali-hk-01", ReleaseVersion: "v0.4.2",
			DownDuration: 12*time.Minute + 34*time.Second,
		},
	}
	if err := th.provider.Send(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	text := th.msgs()[0].Text
	for _, want := range []string{"edge online", "ali-hk-01", "12m34s", "v0.4.2"} {
		if !strings.Contains(text, want) {
			t.Errorf("all-clear is missing %q:\n%s", want, text)
		}
	}
	t.Logf("rendered message:\n%s", text)
}

// Behavior: an edge that has not been seen since main started has no version
// to report, and the message says nothing rather than something empty.
func TestTelegramOmitsAnUnknownEdgeVersion(t *testing.T) {
	th := newTelegramHarness(t)
	ev := Event{
		Kind: model.EventEdgeOffline,
		Node: &NodeEvent{Type: NodeEdge, Name: "never-seen", DownDuration: time.Hour},
	}
	if err := th.provider.Send(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if text := th.msgs()[0].Text; strings.Contains(text, "version") {
		t.Errorf("no version is known, none should be claimed:\n%s", text)
	}
}

// Behavior: node fields are escaped too. Server names come from
// hookploy.yaml rather than from a payload, but the rule is every
// interpolated value — an exception here is one a future field inherits.
func TestTelegramEscapesNodeFields(t *testing.T) {
	th := newTelegramHarness(t)
	ev := Event{
		Kind: model.EventEdgeOffline,
		Node: &NodeEvent{Type: NodeEdge, Name: "a<b>", ReleaseVersion: "v1&2"},
	}
	if err := th.provider.Send(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	text := th.msgs()[0].Text
	if strings.Contains(text, "a<b>") || strings.Contains(text, "v1&2") {
		t.Errorf("node fields were not escaped:\n%s", text)
	}
	if !strings.Contains(text, "a&lt;b&gt;") || !strings.Contains(text, "v1&amp;2") {
		t.Errorf("escaped forms missing:\n%s", text)
	}
}

// Behavior: providerFor knows telegram and refuses anything else, so a
// backend that is named but not built cannot be silently treated as working.
func TestProviderForResolvesOnlyImplementedBackends(t *testing.T) {
	p, err := providerFor(config.Notify{
		Provider: config.ProviderTelegram,
		Telegram: config.Telegram{BotToken: "t", ChatID: "c"},
	}, nil)
	if err != nil || p == nil {
		t.Fatalf("telegram must resolve: %v", err)
	}
	if _, err := providerFor(config.Notify{Provider: "center"}, nil); err == nil {
		t.Error("an unimplemented provider must not resolve")
	}
}

// Behavior: the whole chain works against a real HTTP endpoint — a settled
// deploy handed to the Hub comes out the other side as a Bot API call. The
// other tests each swap out a link; this one keeps them all.
func TestHubDeliversThroughTheRealTelegramProvider(t *testing.T) {
	th := newTelegramHarness(t)
	h := newHarness(t, telegramOn+"  base_url: https://deploy.example.com\n", "")
	h.hub.newProvider = func(n config.Notify) (Provider, error) {
		p, err := providerFor(n, th.provider.http)
		if err != nil {
			return nil, err
		}
		p.(*telegramProvider).apiBase = th.provider.apiBase
		return p, nil
	}
	h.hub.Start()

	d := h.settle("web", model.StatusFailed, model.StatusCanceled)
	h.hub.Notify(d.ID)
	waitFor(t, "the Bot API call", func() bool { return len(th.msgs()) == 1 })

	msg := th.msgs()[0]
	if path := th.paths()[0]; !strings.Contains(path, "/sendMessage") {
		t.Errorf("posted to %q", path)
	}
	if !strings.Contains(msg.Text, "deploy failed") || !strings.Contains(msg.Text, "web") {
		t.Errorf("message does not report the failure:\n%s", msg.Text)
	}
	if !strings.Contains(msg.Text, "https://deploy.example.com/ui/deploys/"+d.ID) {
		t.Errorf("message is missing the deploy link:\n%s", msg.Text)
	}
	t.Logf("rendered message:\n%s", msg.Text)
}

// Behavior: the pieces cmd_main wires together turn a server nobody attached
// for into a real Bot API call. The watcher, the Hub and the renderer are
// each covered on their own; this is the composition between them, which is
// the part only main is responsible for getting right.
func TestWatcherOutageReachesTheBotAPI(t *testing.T) {
	th := newTelegramHarness(t)
	h := newHarness(t, telegramOn+"  edge_offline_after: 10ms\n", "")
	h.hub.newProvider = func(n config.Notify) (Provider, error) {
		p, err := providerFor(n, th.provider.http)
		if err != nil {
			return nil, err
		}
		p.(*telegramProvider).apiBase = th.provider.apiBase
		return p, nil
	}
	h.hub.Start()

	attached := map[string]model.EdgeInfo{}
	w := &edgewatch.Watcher{
		Config:    h.config,
		Edges:     func() map[string]model.EdgeInfo { return attached },
		Interval:  2 * time.Millisecond,
		OnOffline: func(o edgewatch.Outage) { h.hub.EdgeOffline(o.Server, o.Version, o.Duration) },
		OnOnline:  func(o edgewatch.Outage) { h.hub.EdgeOnline(o.Server, o.Version, o.Duration) },
	}
	w.Start()
	defer w.Shutdown()

	waitFor(t, "the outage to reach the Bot API", func() bool { return len(th.msgs()) == 1 })
	text := th.msgs()[0].Text
	if !strings.Contains(text, "edge offline") || !strings.Contains(text, "edge-01") {
		t.Errorf("message does not report the outage:\n%s", text)
	}
	t.Logf("rendered message:\n%s", text)
}

// Behavior: a transport failure never reveals the bot token. The Bot API puts
// the token in the URL path and Go's *url.Error prints the whole URL, so the
// first network blip would otherwise write the credential straight into
// main's log — where the retry loop prints every failed attempt.
func TestTelegramTransportErrorsHideTheToken(t *testing.T) {
	const secret = "999:SUPERSECRETTOKEN"
	p := &telegramProvider{
		creds: config.Telegram{BotToken: secret, ChatID: "-100"},
		http:  &http.Client{Timeout: 50 * time.Millisecond},
		// Reserved as "invalid, never routable" by RFC 5737.
		apiBase: "http://192.0.2.1:9",
	}
	err := p.Send(context.Background(), failedEvent())
	if err == nil {
		t.Fatal("an unreachable endpoint must be an error so the Hub retries")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the bot token leaked into the error text: %v", err)
	}
	t.Logf("scrubbed error: %v", err)
}
