package model

import (
	"regexp"
	"testing"
	"time"
)

// Behavior: deploy/execution IDs carry their prefix, a hex millisecond
// timestamp and a random tail, and never collide in quick succession.
func TestNewIDFormat(t *testing.T) {
	re := regexp.MustCompile(`^dp_[0-9a-f]{11,}[0-9a-f]{8}$`)
	id := NewDeployID()
	if !re.MatchString(id) {
		t.Fatalf("deploy id %q does not match expected format", id)
	}
	ex := NewExecutionID()
	if ex[:3] != "ex_" {
		t.Fatalf("execution id %q lacks ex_ prefix", ex)
	}
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewDeployID()
		if seen[id] {
			t.Fatalf("duplicate id generated: %s", id)
		}
		seen[id] = true
	}
}

// Behavior: Duration round-trips through YAML/JSON as a human string.
func TestDurationText(t *testing.T) {
	d := Duration(10 * time.Minute)
	b, err := d.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "10m0s" {
		t.Fatalf("got %q", b)
	}
	var d2 Duration
	if err := d2.UnmarshalText([]byte("3s")); err != nil {
		t.Fatal(err)
	}
	if time.Duration(d2) != 3*time.Second {
		t.Fatalf("got %v", d2)
	}
}

// Behavior: a deploy's status is the pure aggregation of its executions.
func TestAggregateStatus(t *testing.T) {
	cases := []struct {
		name string
		in   []Status
		want Status
	}{
		{"all succeeded", []Status{StatusSucceeded, StatusSucceeded}, StatusSucceeded},
		{"any failed", []Status{StatusSucceeded, StatusFailed}, StatusFailed},
		{"any unreachable counts failed", []Status{StatusUnreachable, StatusSucceeded}, StatusFailed},
		{"running wins over queued", []Status{StatusRunning, StatusQueued}, StatusRunning},
		{"dispatching maps to running", []Status{StatusDispatching, StatusQueued}, StatusRunning},
		{"all queued", []Status{StatusQueued, StatusQueued}, StatusQueued},
		{"failed beats running", []Status{StatusFailed, StatusRunning}, StatusFailed},
		{"canceled with success = failed", []Status{StatusSucceeded, StatusCanceled}, StatusFailed},
		{"empty", nil, StatusQueued},
	}
	for _, c := range cases {
		if got := AggregateStatus(c.in); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}
