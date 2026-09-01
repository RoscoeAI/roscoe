package quorum

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/loop"
	"roscoe.sh/roscoe/internal/streamjson"
)

// voterSrv is a fake Anthropic-protocol endpoint that answers with a scripted
// verdict, so the whole quorum can be exercised without a network or a key.
type voterSrv struct {
	*httptest.Server
	calls   atomic.Int32
	lastReq atomic.Value // string: the prompt it was sent
}

func newVoter(t *testing.T, reply func(n int) (int, string)) *voterSrv {
	t.Helper()
	v := &voterSrv{}
	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(v.calls.Add(1))
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) > 0 {
			v.lastReq.Store(body.Messages[0].Content)
		}
		code, text := reply(n)
		if code != 200 {
			w.WriteHeader(code)
			fmt.Fprint(w, text)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]string{{"type": "text", "text": text}},
		})
	}))
	t.Cleanup(v.Close)
	return v
}

// says builds a voter that always returns the same verdict.
func says(t *testing.T, action string, conf float64, kinds ...string) *voterSrv {
	t.Helper()
	k, _ := json.Marshal(kinds)
	if kinds == nil {
		k = []byte("[]")
	}
	return newVoter(t, func(int) (int, string) {
		return 200, fmt.Sprintf(`{"action":%q,"confidence":%v,"reason":"because","kinds":%s}`, action, conf, k)
	})
}

// build wires a Quorum whose voters point at the given fake servers.
func build(t *testing.T, autonomy int, minConf float64, always []string, srvs ...*voterSrv) *Quorum {
	t.Helper()
	provs := map[string]config.Provider{}
	var voters []config.Voter
	for i, s := range srvs {
		name := fmt.Sprintf("p%d", i)
		provs[name] = config.Provider{Protocol: "anthropic", BaseURL: s.URL, Auth: "static:tok"}
		voters = append(voters, config.Voter{Provider: name, Model: fmt.Sprintf("m%d", i)})
	}
	return &Quorum{
		Voters: voters, Providers: provs, Rule: "majority",
		MinConfidence: minConf, AlwaysEscalate: always, AutonomyLevel: autonomy,
		Client: &http.Client{Timeout: 5 * time.Second},
	}
}

func iter(n int) loop.Iteration {
	return loop.Iteration{
		N: n, Charter: "ship the thing",
		LoopMD: "# ship the thing\n\n## Status\ndone\n\n## Tried\n- did a thing\n",
		Status: loop.StatusDone,
		Result: &streamjson.ResultEvent{Result: "I did the thing."},
	}
}

func decide(t *testing.T, q *Quorum) loop.Decision {
	t.Helper()
	d, err := q.Decide(context.Background(), iter(1))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	return d
}

func TestMajorityRules(t *testing.T) {
	q := build(t, 90, 0, nil, says(t, "done", 1), says(t, "done", 1), says(t, "continue", 1))
	if d := decide(t, q); d.Action != loop.Done {
		t.Errorf("action = %s, want done (%s)", d.Action, d.Reason)
	}

	q = build(t, 90, 0, nil, says(t, "continue", 1), says(t, "continue", 1), says(t, "done", 1))
	d := decide(t, q)
	if d.Action != loop.Continue {
		t.Errorf("action = %s, want continue", d.Action)
	}
	if d.Prompt == "" {
		t.Error("a continue decision must carry the next prompt")
	}
}

// A split is exactly the case the quorum should not resolve on its own.
func TestSplitEscalates(t *testing.T) {
	q := build(t, 90, 0, nil, says(t, "done", 1), says(t, "continue", 1))
	d := decide(t, q)
	if d.Action != loop.Escalate {
		t.Errorf("action = %s, want escalate on a 1-1 split", d.Action)
	}
	if !strings.Contains(d.Reason, "no majority") {
		t.Errorf("reason %q should explain the split", d.Reason)
	}
}

// The worker grading its own work is the thing the quorum exists to fix: a
// loop.md saying done must not carry the vote.
func TestWorkerClaimDoesNotDecide(t *testing.T) {
	q := build(t, 90, 0, nil, says(t, "continue", 1), says(t, "continue", 1))
	it := iter(1) // its loop.md and Status both say done
	d, err := q.Decide(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != loop.Continue {
		t.Errorf("action = %s; the voters said continue and they outrank the worker", d.Action)
	}
}

func TestBelowConfidenceFloorEscalates(t *testing.T) {
	q := build(t, 90, 0.7, nil, says(t, "done", 0.4), says(t, "done", 0.5))
	d := decide(t, q)
	if d.Action != loop.Escalate {
		t.Errorf("action = %s, want escalate below the floor", d.Action)
	}
	if !strings.Contains(d.Reason, "0.70") {
		t.Errorf("reason %q should name the floor", d.Reason)
	}

	// Comfortably above the floor, the same votes stand.
	q = build(t, 90, 0.7, nil, says(t, "done", 0.9), says(t, "done", 0.95))
	if d := decide(t, q); d.Action != loop.Done {
		t.Errorf("action = %s, want done above the floor", d.Action)
	}
}

// Autonomy 100 means never ask. An unresolvable vote becomes more work, not a
// question, and the loop's budget and ceiling remain the real backstops.
func TestAutonomy100NeverAsks(t *testing.T) {
	for _, tc := range []struct {
		name string
		q    *Quorum
	}{
		{"split", build(t, 100, 0, nil, says(t, "done", 1), says(t, "continue", 1))},
		{"low confidence", build(t, 100, 0.9, nil, says(t, "done", 0.2), says(t, "done", 0.2))},
		{"no voter answered", build(t, 100, 0, nil, newVoter(t, func(int) (int, string) { return 500, "boom" }))},
	} {
		d := decide(t, tc.q)
		if d.Action != loop.Continue {
			t.Errorf("%s: action = %s, want continue at autonomy 100 (%s)", tc.name, d.Action, d.Reason)
		}
		if !strings.Contains(d.Reason, "autonomy is 100") {
			t.Errorf("%s: reason %q should say why it did not ask", tc.name, d.Reason)
		}
	}
}

// Below 100 the same situations do reach the human.
func TestBelowFullAutonomyAsks(t *testing.T) {
	for _, tc := range []struct {
		name string
		q    *Quorum
	}{
		{"split", build(t, 99, 0, nil, says(t, "done", 1), says(t, "continue", 1))},
		{"no voter answered", build(t, 0, 0, nil, newVoter(t, func(int) (int, string) { return 500, "boom" }))},
	} {
		if d := decide(t, tc.q); d.Action != loop.Escalate {
			t.Errorf("%s: action = %s, want escalate below autonomy 100", tc.name, d.Action)
		}
	}
}

// The safety list outranks the dial. A destructive action at autonomy 100 must
// still stop, or always_escalate is dead config exactly when it matters.
func TestAlwaysEscalateOutranksAutonomy(t *testing.T) {
	always := []string{"destructive-actions", "spend-over-usd:20"}
	q := build(t, 100, 0, always,
		says(t, "continue", 1, "destructive-actions"),
		says(t, "continue", 1, "destructive-actions"),
		says(t, "continue", 1))
	d := decide(t, q)
	if d.Action != loop.Escalate {
		t.Fatalf("action = %s, want escalate; always_escalate must beat autonomy 100", d.Action)
	}
	if !strings.Contains(d.Reason, "destructive-actions") {
		t.Errorf("reason %q should name the kind", d.Reason)
	}
}

// One voter flagging it is not a quorum; the list needs a majority too, or a
// single hallucinated category halts every run.
func TestOneFlagIsNotEnough(t *testing.T) {
	q := build(t, 90, 0, []string{"destructive-actions"},
		says(t, "continue", 1, "destructive-actions"),
		says(t, "continue", 1),
		says(t, "continue", 1))
	if d := decide(t, q); d.Action != loop.Continue {
		t.Errorf("action = %s, want continue; one flag of three is not a majority", d.Action)
	}
}

// Voters phrase categories loosely; a safety list defeated by capitalisation
// is not a safety list.
func TestKindMatchingIsForgiving(t *testing.T) {
	q := build(t, 100, 0, []string{"spend-over-usd:20"},
		says(t, "continue", 1, "Spend Over USD"),
		says(t, "continue", 1, "spend-over-usd:20"))
	if d := decide(t, q); d.Action != loop.Escalate {
		t.Errorf("action = %s, want escalate; %q should match the configured kind", d.Action, "Spend Over USD")
	}
}

// A slow or broken endpoint must not take the quorum down with it.
func TestOneVoterFailingIsNotFatal(t *testing.T) {
	broken := newVoter(t, func(int) (int, string) { return 503, "unavailable" })
	q := build(t, 90, 0, nil, says(t, "done", 1), says(t, "done", 1), broken)
	d := decide(t, q)
	if d.Action != loop.Done {
		t.Errorf("action = %s, want done from the two that answered", d.Action)
	}
	if !strings.Contains(d.Reason, "2 of 2") {
		t.Errorf("reason %q should count only the votes cast", d.Reason)
	}
}

// A failed iteration is not a judgment call, and putting it to a vote spends
// money to be told what the loop already knows.
func TestFailedIterationSkipsTheVote(t *testing.T) {
	v := says(t, "done", 1)
	q := build(t, 90, 0, nil, v)
	it := iter(1)
	it.Err = fmt.Errorf("worker exploded")
	d, err := q.Decide(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != loop.Continue {
		t.Errorf("action = %s, want continue", d.Action)
	}
	if v.calls.Load() != 0 {
		t.Errorf("polled %d voters on a failed iteration; want 0", v.calls.Load())
	}
	if !strings.Contains(d.Prompt, "worker exploded") {
		t.Error("the retry prompt should carry the failure")
	}
}

// Wall clock must be the slowest voter, not the sum of them.
func TestVotersRunInParallel(t *testing.T) {
	const delay = 150 * time.Millisecond
	slow := func() *voterSrv {
		return newVoter(t, func(int) (int, string) {
			time.Sleep(delay)
			return 200, `{"action":"done","confidence":1,"reason":"ok","kinds":[]}`
		})
	}
	q := build(t, 90, 0, nil, slow(), slow(), slow(), slow())
	start := time.Now()
	decide(t, q)
	if elapsed := time.Since(start); elapsed > 3*delay {
		t.Errorf("4 voters took %s; serial would be ~%s, parallel ~%s", elapsed, 4*delay, delay)
	}
}

func TestBallotCarriesTheEvidence(t *testing.T) {
	v := says(t, "done", 1)
	q := build(t, 90, 0, []string{"destructive-actions"}, v)
	decide(t, q)
	sent, _ := v.lastReq.Load().(string)
	for _, want := range []string{"ship the thing", "did a thing", "I did the thing.", "destructive-actions", "iteration 1"} {
		if !strings.Contains(strings.ToLower(sent), strings.ToLower(want)) {
			t.Errorf("the ballot never mentions %q", want)
		}
	}
	if !strings.Contains(sent, "claim to check") {
		t.Error("the ballot should tell the voter not to trust the worker's own status")
	}
}

func TestUnknownProviderAndBadAuthAreVoterErrors(t *testing.T) {
	q := &Quorum{
		Voters:    []config.Voter{{Provider: "nope", Model: "m"}},
		Providers: map[string]config.Provider{},
		Rule:      "majority", AutonomyLevel: 0,
	}
	if d := decide(t, q); d.Action != loop.Escalate {
		t.Errorf("action = %s, want escalate when no voter can be reached", d.Action)
	}

	q = &Quorum{
		Voters:    []config.Voter{{Provider: "p", Model: "m"}},
		Providers: map[string]config.Provider{"p": {BaseURL: "http://127.0.0.1:1", Auth: "env:NOT_SET_ANYWHERE"}},
		Rule:      "majority", AutonomyLevel: 0,
	}
	if d := decide(t, q); d.Action != loop.Escalate {
		t.Errorf("action = %s, want escalate when auth cannot be resolved", d.Action)
	}
}

func TestNewFromConfig(t *testing.T) {
	cfg := config.Default()
	q := New(cfg, map[string]string{"DEEP_INFRA_API_KEY": "k"})
	if len(q.Voters) != len(cfg.Quorum.Voters) {
		t.Errorf("got %d voters, want %d", len(q.Voters), len(cfg.Quorum.Voters))
	}
	if q.Rule != cfg.Quorum.Decide || q.MinConfidence != cfg.Quorum.MinConfidence {
		t.Error("decision rule or confidence floor did not carry over")
	}
	if q.AutonomyLevel != cfg.Autonomy.Level {
		t.Errorf("autonomy = %d, want %d", q.AutonomyLevel, cfg.Autonomy.Level)
	}
	// It must satisfy the interface it exists to implement.
	var _ loop.Judge = q
}
