package models

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// fakeHarness stands in for the claude binary: it POSTs the concrete id the
// real one would put on the wire for a given alias.
func fakeHarness(resolve map[string]string) probeRunner {
	return func(ctx context.Context, bin, base, alias string) error {
		concrete, ok := resolve[alias]
		if !ok {
			return errors.New("unknown alias, harness exited without sending")
		}
		body := fmt.Sprintf(`{"model":%q,"max_tokens":1,"messages":[]}`, concrete)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages?beta=true", bytes.NewReader([]byte(body)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}
}

func TestResolveViaHarnessLearnsWhatTheWireCarries(t *testing.T) {
	c := Open(t.TempDir())
	got, err := c.resolveViaHarness(context.Background(), "anthropic", "claude",
		[]string{"opus", "sonnet"},
		fakeHarness(map[string]string{"opus": "claude-opus-5", "sonnet": "claude-sonnet-5"}))
	if err != nil {
		t.Fatal(err)
	}
	if got["opus"] != "claude-opus-5" || got["sonnet"] != "claude-sonnet-5" {
		t.Errorf("resolved = %v", got)
	}
	// It must land in the catalogue, not just be returned.
	if c.Describe("anthropic", "opus") != "opus  →  claude-opus-5" {
		t.Errorf("describe = %q", c.Describe("anthropic", "opus"))
	}
}

// A concrete id has nothing to learn; probing it would only spend time.
func TestResolveViaHarnessSkipsConcreteIds(t *testing.T) {
	calls := 0
	run := func(ctx context.Context, bin, base, alias string) error {
		calls++
		return nil
	}
	c := Open(t.TempDir())
	got, err := c.resolveViaHarness(context.Background(), "anthropic", "claude",
		[]string{"claude-sonnet-5", "zai-org/GLM-5.3-Flash", "  "}, run)
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
	if calls != 0 {
		t.Errorf("probed %d concrete ids", calls)
	}
}

// One alias the harness cannot resolve must not poison the others.
func TestResolveViaHarnessToleratesOneFailure(t *testing.T) {
	c := Open(t.TempDir())
	got, err := c.resolveViaHarness(context.Background(), "anthropic", "claude",
		[]string{"opus", "nonsense"},
		fakeHarness(map[string]string{"opus": "claude-opus-5"}))
	if err != nil {
		t.Fatalf("one failure became an error: %v", err)
	}
	if got["opus"] != "claude-opus-5" || len(got) != 1 {
		t.Errorf("got %v", got)
	}
}

// If nothing resolved, the caller deserves the reason.
func TestResolveViaHarnessReportsTotalFailure(t *testing.T) {
	c := Open(t.TempDir())
	_, err := c.resolveViaHarness(context.Background(), "anthropic", "claude",
		[]string{"opus"}, fakeHarness(map[string]string{}))
	if err == nil {
		t.Error("total failure returned no error")
	}
}

// A stuck harness must not hang the refresh.
func TestResolveViaHarnessIsTimeBoxed(t *testing.T) {
	stuck := func(ctx context.Context, bin, base, alias string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	c := Open(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _ = c.resolveViaHarness(ctx, "anthropic", "claude", []string{"opus"}, stuck)
	if time.Since(start) > 2*time.Second {
		t.Error("a stuck probe was not bounded by the context")
	}
}

// The capture must refuse in the API's own error shape and remember only the
// first model it saw.
func TestCaptureRefusesAndRecordsFirst(t *testing.T) {
	cap := newCapture()
	defer cap.Close()
	for _, m := range []string{"claude-opus-5", "claude-sonnet-5"} {
		resp, err := http.Post(cap.URL()+"/v1/messages", "application/json",
			bytes.NewReader([]byte(fmt.Sprintf(`{"model":%q}`, m))))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 so the harness stops rather than retries", resp.StatusCode)
		}
		resp.Body.Close()
	}
	if cap.model() != "claude-opus-5" {
		t.Errorf("recorded %q, want the first", cap.model())
	}
	cap.reset()
	if cap.model() != "" {
		t.Error("reset did not clear")
	}
}
