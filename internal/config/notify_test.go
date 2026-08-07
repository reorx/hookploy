package config

import (
	"strings"
	"testing"
	"time"

	"github.com/reorx/hookploy/internal/model"
)

// notifyYAML wraps a notify block and one service around the minimal server
// set, so each test only spells out the part it is about.
func notifyYAML(notify, svcExtra string) string {
	return minimalServers + notify + `
services:
  web:
    server: s1
    dir: /opt/web
` + svcExtra + `    deploy:
      - run: { argv: [deploy] }
`
}

// Behavior: notify is off unless a provider is named, and a config that says
// nothing about notifications still resolves every service to the default
// vocabulary — so turning notify on later needs no per-service edits.
func TestNotifyDefaultsToOffWithFailedOnlyVocabulary(t *testing.T) {
	cfg, err := load(t, notifyYAML("", ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.Provider != "" {
		t.Errorf("provider = %q, want empty (off)", cfg.Notify.Provider)
	}
	web := cfg.Services["web"].Notify
	if !web.Enabled {
		t.Error("a service with no notify block should be enabled")
	}
	if !web.Wants(model.EventDeployFailed) {
		t.Error("deploy.failed should be in the default vocabulary")
	}
	for _, k := range []model.EventKind{model.EventDeploySucceeded, model.EventDeployRecovered, model.EventDeployUnreachable} {
		if web.Wants(k) {
			t.Errorf("%q should be off by default", k)
		}
	}
}

// Behavior: a telegram provider without credentials fails the load. A
// notification channel that is configured but silently dead is worse than
// one that is plainly off, so this must surface at `hookploy validate` time.
func TestNotifyTelegramRequiresCredentials(t *testing.T) {
	cases := []struct {
		name  string
		creds string
	}{
		{"no telegram block", ""},
		{"token only", "  telegram:\n    bot_token: t\n"},
		{"chat id only", "  telegram:\n    chat_id: c\n"},
	}
	for _, c := range cases {
		_, err := load(t, notifyYAML("notify:\n  provider: telegram\n"+c.creds, ""))
		if err == nil {
			t.Errorf("%s: expected an error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), "bot_token") {
			t.Errorf("%s: error should name the missing field, got %v", c.name, err)
		}
	}
}

// Behavior: provider: center is rejected with "not implemented yet" rather
// than a generic bad-value error — the value is real and coming, it just is
// not wired up in this build.
func TestNotifyCenterProviderRejectedAsUnimplemented(t *testing.T) {
	_, err := load(t, notifyYAML("notify:\n  provider: center\n", ""))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("error should say the provider is not implemented yet, got %v", err)
	}
}

// Behavior: a typo in an event name fails the load and names the offender,
// rather than silently subscribing to nothing.
func TestNotifyUnknownEventRejected(t *testing.T) {
	_, err := load(t, notifyYAML("notify:\n  events: [deploy.faild]\n", ""))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "deploy.faild") {
		t.Errorf("error should name the unknown event, got %v", err)
	}
}

// Behavior: the same validation applies to a service-level override, and its
// error points at the service.
func TestServiceNotifyUnknownEventRejected(t *testing.T) {
	_, err := load(t, notifyYAML("", "    notify:\n      events: [nope]\n"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "web") {
		t.Errorf("error should name both the service and the unknown event, got %v", err)
	}
}

// Behavior: an unknown key inside a service's notify block is rejected. The
// service decoder is hand-written, so a nested block only gets the strict
// check if it is explicitly given one.
func TestServiceNotifyRejectsUnknownField(t *testing.T) {
	_, err := load(t, notifyYAML("", "    notify:\n      evnets: [deploy.failed]\n"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "evnets") {
		t.Errorf("error should name the unknown field, got %v", err)
	}
}

// Behavior: enabled: false mutes a service outright, whatever the global
// vocabulary says, and leaves its siblings untouched.
func TestServiceNotifyEnabledFalseMutesThatServiceOnly(t *testing.T) {
	yaml := minimalServers + `
notify:
  provider: telegram
  telegram: { bot_token: t, chat_id: c }
services:
  web:
    server: s1
    dir: /opt/web
    notify:
      enabled: false
    deploy:
      - run: { argv: [deploy] }
  api:
    server: s1
    dir: /opt/api
    deploy:
      - run: { argv: [deploy] }
`
	cfg, err := load(t, yaml)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Services["web"].Notify.Wants(model.EventDeployFailed) {
		t.Error("a muted service must want nothing")
	}
	if !cfg.Services["api"].Notify.Wants(model.EventDeployFailed) {
		t.Error("muting one service must not affect its siblings")
	}
}

// Behavior: a service's events list replaces the global one rather than
// adding to it, so an override can make a service quieter as well as louder.
func TestServiceNotifyEventsReplaceRatherThanMerge(t *testing.T) {
	yaml := minimalServers + `
notify:
  events: [deploy.failed]
services:
  loud:
    server: s1
    dir: /opt/loud
    notify:
      events: [deploy.succeeded, deploy.recovered]
    deploy:
      - run: { argv: [deploy] }
`
	cfg, err := load(t, yaml)
	if err != nil {
		t.Fatal(err)
	}
	n := cfg.Services["loud"].Notify
	if n.Wants(model.EventDeployFailed) {
		t.Error("an override replaces the global list; deploy.failed should be gone")
	}
	if !n.Wants(model.EventDeploySucceeded) || !n.Wants(model.EventDeployRecovered) {
		t.Errorf("override list not applied: %+v", n.Events)
	}
}

// Behavior: an explicitly empty events list is distinguishable from an
// omitted one — it silences the service instead of falling back to defaults.
func TestNotifyExplicitEmptyEventsIsNotTheDefault(t *testing.T) {
	cfg, err := load(t, notifyYAML("notify:\n  events: []\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Notify.Events) != 0 {
		t.Errorf("explicit [] should stay empty, got %v", cfg.Notify.Events)
	}
	if cfg.Services["web"].Notify.Wants(model.EventDeployFailed) {
		t.Error("an explicitly empty global list should leave services wanting nothing")
	}
}

// Behavior: the default vocabulary subscribes to every node event. Those
// fire at most once per main restart and once per edge outage, and an
// install that turned notifications on wants to hear that its fleet went
// dark without editing a list first.
func TestNotifyDefaultsIncludeEveryNodeEvent(t *testing.T) {
	cfg, err := load(t, notifyYAML("", ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range model.EventKinds() {
		if k.Scope() != model.ScopeNode {
			continue
		}
		if !cfg.Notify.Wants(k) {
			t.Errorf("%q should be on by default", k)
		}
	}
	if cfg.Notify.Wants(model.EventDeploySucceeded) {
		t.Error("deploy.succeeded must stay opt-in; it fires on every green deploy")
	}
}

// Behavior: the edge-offline threshold is a duration with a default, so an
// install gets outage alerts without configuring anything, and tuning it is
// the same `5m` spelling as every other duration in the file.
func TestNotifyEdgeOfflineAfterDefaultsAndParses(t *testing.T) {
	cfg, err := load(t, notifyYAML("", ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.EdgeOfflineAfter != defaultEdgeOfflineAfter {
		t.Errorf("edge_offline_after = %s, want the %s default", cfg.Notify.EdgeOfflineAfter, defaultEdgeOfflineAfter)
	}

	cfg, err = load(t, notifyYAML("notify:\n  edge_offline_after: 90s\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.EdgeOfflineAfter != 90*time.Second {
		t.Errorf("edge_offline_after = %s, want 90s", cfg.Notify.EdgeOfflineAfter)
	}
}

// Behavior: a negative threshold fails the load. It parses as a duration and
// would leave outage alerting silently off — the same way a telegram block
// missing its credentials would leave the whole channel silently off.
func TestNotifyEdgeOfflineAfterRejectsANegativeThreshold(t *testing.T) {
	_, err := load(t, notifyYAML("notify:\n  edge_offline_after: -1m\n", ""))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "edge_offline_after") {
		t.Errorf("error should name the field, got %v", err)
	}
}

// Behavior: a service asking for a node event fails the load. A service has
// no say in whether main restarting is worth a message, so accepting the key
// would produce a config that reads like it does something it cannot.
func TestServiceNotifyRejectsNodeScopedEvent(t *testing.T) {
	_, err := load(t, notifyYAML("", "    notify:\n      events: [deploy.failed, edge.offline]\n"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "edge.offline") || !strings.Contains(err.Error(), "web") {
		t.Errorf("error should name both the service and the offending event, got %v", err)
	}
}

// Behavior: node events never reach a service's policy, not even by
// inheritance from the global default list — ServiceNotify.Wants is asked
// only about deploys, and answering true for edge.offline would be a lie
// waiting for a caller to believe it.
func TestServiceNotifyNeverInheritsNodeScopedEvents(t *testing.T) {
	cfg, err := load(t, notifyYAML("", ""))
	if err != nil {
		t.Fatal(err)
	}
	web := cfg.Services["web"].Notify
	for _, k := range model.EventKinds() {
		if k.Scope() == model.ScopeNode && web.Wants(k) {
			t.Errorf("service policy wants %q, but node events are not a service's business", k)
		}
	}
	if !web.Wants(model.EventDeployFailed) {
		t.Error("filtering node events must not drop the deploy ones alongside them")
	}
}

// Behavior: base_url is stored without its trailing slash so link building
// stays a plain concatenation, and credentials survive normalization.
func TestNotifyTelegramAndBaseURLNormalize(t *testing.T) {
	cfg, err := load(t, notifyYAML(
		"notify:\n  provider: telegram\n  base_url: https://deploy.example.com/\n"+
			"  telegram:\n    bot_token: tok\n    chat_id: \"-100123\"\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.BaseURL != "https://deploy.example.com" {
		t.Errorf("base_url = %q, want the trailing slash trimmed", cfg.Notify.BaseURL)
	}
	if cfg.Notify.Telegram.BotToken != "tok" || cfg.Notify.Telegram.ChatID != "-100123" {
		t.Errorf("telegram credentials lost: %+v", cfg.Notify.Telegram)
	}
}
