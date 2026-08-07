package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/model"
)

const telegramAPIBase = "https://api.telegram.org"

// HTTPDoer is the injectable HTTP client, same shape and same reason as
// engine.HTTPDoer: tests run without a network.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

var defaultHTTPClient = &http.Client{Timeout: 15 * time.Second}

// providerFor resolves the configured backend. The notification-center
// backend lands as one more case here and nowhere else in this package.
func providerFor(n config.Notify, doer HTTPDoer) (Provider, error) {
	switch n.Provider {
	case config.ProviderTelegram:
		return &telegramProvider{creds: n.Telegram, http: doer, apiBase: telegramAPIBase}, nil
	default:
		return nil, fmt.Errorf("no provider for %q", n.Provider)
	}
}

// telegramProvider posts one Bot API sendMessage per event. Credentials come
// from the config snapshot the Hub read for this attempt, so a rotated token
// takes effect on the next retry without restarting main.
type telegramProvider struct {
	creds   config.Telegram
	http    HTTPDoer
	apiBase string
}

// sendMessage is the Bot API request body — this provider's own wire type,
// kept here rather than anywhere shared for the same reason internal/edgewire
// sits outside internal/api: it is a private protocol with one backend, free
// to follow that backend's changes.
type sendMessage struct {
	ChatID                string `json:"chat_id"`
	Text                  string `json:"text"`
	ParseMode             string `json:"parse_mode"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
}

func (p *telegramProvider) Send(ctx context.Context, ev Event) error {
	if p.creds.BotToken == "" || p.creds.ChatID == "" {
		return fmt.Errorf("telegram: bot_token/chat_id not configured")
	}
	body, err := json.Marshal(sendMessage{
		ChatID:                p.creds.ChatID,
		Text:                  renderTelegram(ev),
		ParseMode:             "HTML",
		DisableWebPagePreview: true,
	})
	if err != nil {
		return fmt.Errorf("telegram: encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.apiBase+"/bot"+p.creds.BotToken+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram: build request: %w", scrubURL(err))
	}
	req.Header.Set("Content-Type", "application/json")
	doer := p.http
	if doer == nil {
		doer = defaultHTTPClient
	}
	resp, err := doer.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: %w", scrubURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The body carries Telegram's own description of what was wrong,
		// which is the difference between a fixable log line and a mystery.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("telegram: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// scrubURL drops the request URL from a transport error. The bot token is
// part of that URL — the Bot API puts it in the path — and *url.Error prints
// the whole thing, so an ordinary timeout would otherwise write the token
// into main's log the moment delivery starts failing.
func scrubURL(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return fmt.Errorf("%s: %w", uerr.Op, uerr.Err)
	}
	return err
}

var eventHeadline = map[model.EventKind]string{
	model.EventDeployFailed:      "\U0001F534 deploy failed",
	model.EventDeployUnreachable: "\U0001F7E0 deploy unreachable",
	model.EventDeployRecovered:   "\U0001F7E2 deploy recovered",
	model.EventDeploySucceeded:   "✅ deploy succeeded",
	model.EventMainStarted:       "\U0001F680 main started",
	model.EventEdgeOffline:       "\U0001F534 edge offline",
	model.EventEdgeOnline:        "\U0001F7E2 edge online",
}

// renderTelegram formats an event as Bot API HTML. HTML parse mode was
// picked over MarkdownV2 because escaping it is three characters that
// html.EscapeString already handles; MarkdownV2's eighteen are a much easier
// thing to get wrong on text that comes straight out of a failing command.
//
// The split mirrors Event's: the two payloads describe different things and
// read nothing alike, so each gets its own renderer rather than one function
// picking its way through fields that half the kinds leave empty.
func renderTelegram(ev Event) string {
	switch {
	case ev.Deploy != nil:
		return renderDeploy(ev.Kind, ev.CreatedAt, ev.Deploy)
	case ev.Node != nil:
		return renderNode(ev.Kind, ev.Node)
	default:
		return eventHeadline[ev.Kind] + "\n"
	}
}

func renderDeploy(kind model.EventKind, createdAt time.Time, d *DeployEvent) string {
	var b strings.Builder
	subject := d.Service
	if d.Task != "" {
		subject += " / " + d.Task
	}
	fmt.Fprintf(&b, "%s\n<b>%s</b>\n", eventHeadline[kind], esc(subject))
	if d.Error != "" {
		fmt.Fprintf(&b, "%s\n", esc(d.Error))
	}
	for _, in := range d.Instances {
		fmt.Fprintf(&b, "• %s (%s) %s", esc(in.Instance), esc(in.Server), in.Status)
		if in.Error != "" {
			fmt.Fprintf(&b, ": %s", esc(in.Error))
		}
		b.WriteByte('\n')
	}
	if !d.FinishedAt.IsZero() && !createdAt.IsZero() {
		fmt.Fprintf(&b, "took %s\n", d.FinishedAt.Sub(createdAt).Round(time.Second))
	}
	if d.URL != "" {
		fmt.Fprintf(&b, "%s\n", esc(d.URL))
	}
	return b.String()
}

// renderNode reports a node's state. Which node it is comes from Type and
// what happened from Kind — main has only ever one thing to say, so its
// version is the whole message, while an edge leads with its name, because
// which one went dark is what anyone reads for first.
func renderNode(kind model.EventKind, n *NodeEvent) string {
	var b strings.Builder
	b.WriteString(eventHeadline[kind])
	b.WriteByte('\n')
	if n.Type == NodeMain {
		fmt.Fprintf(&b, "<b>%s</b>\n", esc(n.ReleaseVersion))
		return b.String()
	}
	fmt.Fprintf(&b, "<b>%s</b>\n", esc(n.Name))
	down := n.DownDuration.Round(time.Second)
	version := "last known version"
	if kind == model.EventEdgeOnline {
		fmt.Fprintf(&b, "back after %s offline\n", down)
		version = "version"
	} else {
		fmt.Fprintf(&b, "offline for %s\n", down)
	}
	if n.ReleaseVersion != "" {
		fmt.Fprintf(&b, "%s %s\n", version, esc(n.ReleaseVersion))
	}
	return b.String()
}

// esc covers the characters Bot API HTML mode reserves (and a couple more,
// which Telegram accepts as entities). Every interpolated value goes through
// it: service names, server names and above all error text, which is
// arbitrary command output.
func esc(s string) string { return html.EscapeString(s) }
