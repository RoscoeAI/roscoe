// Package quorum decides what happens after a loop iteration by asking
// several models instead of taking the worker at its word. It implements
// loop.Judge, replacing StatusJudge, whose weakness is structural: the model
// that did the work also grades it.
//
// Voters are called directly rather than through the router. The router exists
// to redirect a claude harness that can only be pointed at one base URL; Go
// code has no such constraint and gains nothing from the indirection.
package quorum

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/loop"
)

// Verdict is one voter's answer, or its failure.
type Verdict struct {
	Voter      string // "provider/model", for the ledger
	Action     loop.Action
	Confidence float64
	Reason     string
	// Kinds are escalation categories the voter recognised in this
	// iteration, matched against quorum.always_escalate.
	Kinds []string
	Err   error
}

func (v Verdict) ok() bool { return v.Err == nil && v.Action != "" }

// Quorum implements loop.Judge.
type Quorum struct {
	Voters    []config.Voter
	Providers map[string]config.Provider
	Accounts  []config.Account
	Env       map[string]string
	// Rule is how votes resolve; "majority" is the only rule today.
	Rule           string
	MinConfidence  float64
	AlwaysEscalate []string
	// AutonomyLevel is 0-100. At 100 a decision the quorum is unsure about
	// becomes "keep working" rather than a question for the human. It does
	// not override AlwaysEscalate; see the package tests for why.
	AutonomyLevel int

	Timeout time.Duration
	Client  *http.Client
	// OnVerdicts reports every ballot, including failures, for the ledger.
	OnVerdicts func(loop.Iteration, []Verdict, loop.Decision)
}

// New builds a Quorum from a config.
func New(cfg *config.Config, env map[string]string) *Quorum {
	return &Quorum{
		Voters:         cfg.Quorum.Voters,
		Providers:      cfg.Providers,
		Accounts:       cfg.Accounts,
		Env:            env,
		Rule:           cfg.Quorum.Decide,
		MinConfidence:  cfg.Quorum.MinConfidence,
		AlwaysEscalate: cfg.Quorum.AlwaysEscalate,
		AutonomyLevel:  cfg.Autonomy.Level,
	}
}

const defaultTimeout = 90 * time.Second

func (q *Quorum) client() *http.Client {
	if q.Client != nil {
		return q.Client
	}
	t := q.Timeout
	if t == 0 {
		t = defaultTimeout
	}
	return &http.Client{Timeout: t}
}

// Decide asks every voter in parallel and reduces their answers to one
// decision. A voter that fails is not fatal: the quorum rules on whoever
// answered, because a fleet that stops when one endpoint is slow is not
// autonomous.
func (q *Quorum) Decide(ctx context.Context, it loop.Iteration) (loop.Decision, error) {
	// A failed iteration is not a judgment call; retry it the same way
	// StatusJudge does rather than spending votes on it.
	if it.Err != nil {
		d := loop.Decision{
			Action:     loop.Continue,
			Prompt:     fmt.Sprintf("The previous iteration failed with: %v\n\n%s", it.Err, loop.KernelPrompt(it.Charter, loop.Projection(it.LoopMD, 0))),
			Reason:     "retrying after a failed iteration; not put to a vote",
			Confidence: 0.5,
		}
		q.report(it, nil, d)
		return d, nil
	}

	verdicts := q.poll(ctx, it)
	d := q.tally(it, verdicts)
	q.report(it, verdicts, d)
	return d, nil
}

// poll fans out to every voter at once. Wall-clock is the slowest voter, not
// their sum, which is the difference between a quorum you would leave on and
// one you would turn off.
func (q *Quorum) poll(ctx context.Context, it loop.Iteration) []Verdict {
	out := make([]Verdict, len(q.Voters))
	var wg sync.WaitGroup
	for i, v := range q.Voters {
		wg.Add(1)
		go func(i int, v config.Voter) {
			defer wg.Done()
			out[i] = q.ask(ctx, v, it)
		}(i, v)
	}
	wg.Wait()
	return out
}

func (q *Quorum) tally(it loop.Iteration, verdicts []Verdict) loop.Decision {
	var cast []Verdict
	for _, v := range verdicts {
		if v.ok() {
			cast = append(cast, v)
		}
	}

	// Nobody answered. Below full autonomy that is a question for the human;
	// at 100 it is a reason to keep working, and the loop's budget and
	// iteration ceiling remain the real backstops.
	if len(cast) == 0 {
		return q.unsure(it, "no voter answered")
	}

	// An always-escalate kind outranks the autonomy dial. The dial governs
	// how much judgment the quorum absorbs; this list is the set of things
	// nobody wants absorbed, and a dial at 100 turning off the destructive
	// -action check would make the field dead config exactly when it matters.
	if kind, n := q.flaggedKind(cast); n*2 > len(cast) {
		return loop.Decision{
			Action:     loop.Escalate,
			Reason:     fmt.Sprintf("%d of %d voters flagged %q, which always escalates", n, len(cast), kind),
			Confidence: 1,
		}
	}

	counts := map[loop.Action]int{}
	for _, v := range cast {
		counts[v.Action]++
	}
	winner, votes := plurality(counts)

	// "majority" means more than half of those who answered; anything less is
	// a split the quorum should not resolve on its own.
	if q.Rule == "majority" && votes*2 <= len(cast) {
		return q.unsure(it, fmt.Sprintf("split %s, no majority of %d voters", formatCounts(counts), len(cast)))
	}

	conf := meanConfidence(cast, winner)
	if q.MinConfidence > 0 && conf < q.MinConfidence {
		return q.unsure(it, fmt.Sprintf("%s at %.2f confidence, below the %.2f floor", winner, conf, q.MinConfidence))
	}

	d := loop.Decision{
		Action:     winner,
		Reason:     fmt.Sprintf("%d of %d voters: %s", votes, len(cast), reasonOf(cast, winner)),
		Confidence: conf,
	}
	if winner == loop.Continue {
		d.Prompt = loop.KernelPrompt(it.Charter, loop.Projection(it.LoopMD, 0))
	}
	return d
}

// unsure is what the quorum does when it cannot rule: ask the human, unless
// autonomy is at 100, where the instruction is to never ask.
func (q *Quorum) unsure(it loop.Iteration, why string) loop.Decision {
	if q.AutonomyLevel >= 100 {
		return loop.Decision{
			Action:     loop.Continue,
			Prompt:     loop.KernelPrompt(it.Charter, loop.Projection(it.LoopMD, 0)),
			Reason:     why + "; continuing because autonomy is 100",
			Confidence: 0.5,
		}
	}
	return loop.Decision{Action: loop.Escalate, Reason: why, Confidence: 0.5}
}

// flaggedKind returns the most-flagged always-escalate kind and its count.
func (q *Quorum) flaggedKind(cast []Verdict) (string, int) {
	if len(q.AlwaysEscalate) == 0 {
		return "", 0
	}
	always := map[string]bool{}
	for _, k := range q.AlwaysEscalate {
		always[normalizeKind(k)] = true
	}
	counts := map[string]int{}
	for _, v := range cast {
		seen := map[string]bool{}
		for _, k := range v.Kinds {
			n := normalizeKind(k)
			if always[n] && !seen[n] {
				seen[n] = true
				counts[n]++
			}
		}
	}
	best, bestN := "", 0
	for k, n := range counts {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	return best, bestN
}

// normalizeKind makes "spend-over-usd:20" and "Spend Over USD" comparable, so
// a voter's phrasing does not silently defeat a safety list.
func normalizeKind(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	return strings.Trim(strings.ReplaceAll(s, " ", "-"), "-")
}

func plurality(counts map[loop.Action]int) (loop.Action, int) {
	// Sorted so a tie resolves the same way every run rather than by map
	// iteration order.
	keys := make([]string, 0, len(counts))
	for a := range counts {
		keys = append(keys, string(a))
	}
	sort.Strings(keys)
	var best loop.Action
	bestN := 0
	for _, k := range keys {
		if n := counts[loop.Action(k)]; n > bestN {
			best, bestN = loop.Action(k), n
		}
	}
	return best, bestN
}

func meanConfidence(cast []Verdict, action loop.Action) float64 {
	sum, n := 0.0, 0
	for _, v := range cast {
		if v.Action == action {
			sum += v.Confidence
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

func reasonOf(cast []Verdict, action loop.Action) string {
	for _, v := range cast {
		if v.Action == action && strings.TrimSpace(v.Reason) != "" {
			return oneLine(v.Reason, 160)
		}
	}
	return string(action)
}

func (q *Quorum) report(it loop.Iteration, verdicts []Verdict, d loop.Decision) {
	if q.OnVerdicts != nil {
		q.OnVerdicts(it, verdicts, d)
	}
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func formatCounts(counts map[loop.Action]int) string {
	parts := make([]string, 0, len(counts))
	for a, n := range counts {
		parts = append(parts, fmt.Sprintf("%s=%d", a, n))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

// resolveAuth turns a provider's auth spec into an Authorization header value,
// or "" when the caller should send none.
func (q *Quorum) resolveAuth(p config.Provider, account string) (string, error) {
	switch {
	case strings.HasPrefix(p.Auth, "env:"):
		name := strings.TrimPrefix(p.Auth, "env:")
		if v := q.Env[name]; v != "" {
			return "Bearer " + v, nil
		}
		if v := os.Getenv(name); v != "" {
			return "Bearer " + v, nil
		}
		return "", fmt.Errorf("%s not set in the env file", name)
	case strings.HasPrefix(p.Auth, "static:"):
		return "Bearer " + strings.TrimPrefix(p.Auth, "static:"), nil
	case p.Auth == "account":
		// The supervisor has no harness credentials of its own, so an
		// account-auth voter only works when its account resolves to a token.
		for _, a := range q.Accounts {
			if a.Name != account || (a.Enabled != nil && !*a.Enabled) {
				continue
			}
			name, ok := strings.CutPrefix(a.TokenRef, "env:")
			if !ok {
				return "", fmt.Errorf("account %q is %s, which the supervisor cannot read", account, a.TokenRef)
			}
			if v := q.Env[name]; v != "" {
				return "Bearer " + v, nil
			}
			if v := os.Getenv(name); v != "" {
				return "Bearer " + v, nil
			}
			return "", fmt.Errorf("account %q: %s not set", account, name)
		}
		return "", fmt.Errorf("no enabled account %q", account)
	default:
		return "", fmt.Errorf("unsupported provider auth %q", p.Auth)
	}
}
