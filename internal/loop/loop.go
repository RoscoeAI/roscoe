package loop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"roscoe.sh/roscoe/internal/ledger"
	"roscoe.sh/roscoe/internal/streamjson"
)

// Action is what happens after an iteration.
type Action string

const (
	// Continue dispatches another iteration with Decision.Prompt.
	Continue Action = "continue"
	// Done ends the run successfully.
	Done Action = "done"
	// Escalate ends the run and asks a human. Once the quorum lands, this is
	// what autonomy.level governs: at 100 the quorum answers instead.
	Escalate Action = "escalate"
	// Abort ends the run because continuing would be wrong.
	Abort Action = "abort"
)

// Iteration is what a judge sees: the run so far, plus this turn's outcome.
type Iteration struct {
	N        int    // 1-based
	Charter  string //
	LoopMD   string // the file as the worker left it
	Status   string // parsed from LoopMD
	Result   *streamjson.ResultEvent
	Err      error         // the worker failed; Result may be nil
	SpentUSD float64       // the run's total so far, including this iteration
	Elapsed  time.Duration // this iteration
}

// Decision is a judge's answer.
type Decision struct {
	Action     Action
	Prompt     string // the next iteration's instruction; Continue only
	Reason     string // one line, for the ledger and the operator
	Confidence float64
}

// Judge decides what happens after each iteration. The quorum implements this;
// StatusJudge is the deterministic default until it does.
type Judge interface {
	Decide(ctx context.Context, it Iteration) (Decision, error)
}

// Dispatch runs one iteration. The loop takes it as a function so it can be
// driven by a worker, or by a test, without either knowing about the other.
// Returning a session id lets the loop resume rather than start fresh.
type Dispatch func(ctx context.Context, it Iteration, prompt, resume string) (*streamjson.ResultEvent, string, error)

// Options configures a run.
type Options struct {
	Charter  string
	Dir      string
	TaskID   string
	Dispatch Dispatch
	Judge    Judge
	Ledger   *ledger.Ledger // may be nil

	// MaxIterations bounds the run. Zero means DefaultMaxIterations; a
	// negative value means unbounded, which only budget then stops.
	MaxIterations int
	// BudgetUSD stops the run once the total reaches it. Zero means no
	// ceiling beyond whatever the worker enforces per task.
	BudgetUSD float64
	// MaxConsecutiveErrors stops a run whose worker keeps failing, so a
	// broken setup cannot spend a budget discovering that. Zero means
	// DefaultMaxConsecutiveErrors.
	MaxConsecutiveErrors int

	// Recall is asked, before each dispatch, what the fleet already knows
	// about this charter. Whatever it returns is written into loop.md's
	// RecalledSection for the worker to read, so the worker never learns a
	// graph exists and the codex path works identically. Optional: a nil
	// Recall, an error, or an empty string all mean "no recall this run",
	// never a failed iteration.
	Recall func(ctx context.Context, it Iteration) string
	// Signal reports back whether that recall helped, after the judge rules.
	Signal func(ctx context.Context, it Iteration, d Decision)

	// OnIteration reports progress; may be nil.
	OnIteration func(it Iteration, d Decision)
}

const (
	DefaultMaxIterations        = 12
	DefaultMaxConsecutiveErrors = 3
)

// Summary is how a run ended.
type Summary struct {
	Iterations int
	Action     Action
	Reason     string
	SpentUSD   float64
	Status     string // loop.md's last status
	Session    string // the last worker session, for resuming by hand
}

// Run is the supervisor loop: seed the working memory, dispatch, judge,
// dispatch again. It always returns a Summary describing how it stopped, even
// alongside an error.
func Run(ctx context.Context, o Options) (*Summary, error) {
	if o.Dispatch == nil {
		return nil, errors.New("loop: no dispatch")
	}
	if o.Judge == nil {
		o.Judge = StatusJudge{}
	}
	if o.Dir == "" {
		return nil, errors.New("loop: no working directory")
	}
	if strings.TrimSpace(o.Charter) == "" {
		return nil, errors.New("loop: empty charter")
	}
	if o.MaxIterations == 0 {
		o.MaxIterations = DefaultMaxIterations
	}
	if o.MaxConsecutiveErrors == 0 {
		o.MaxConsecutiveErrors = DefaultMaxConsecutiveErrors
	}

	if seeded, err := EnsureSeeded(o.Dir, o.Charter); err != nil {
		return nil, err
	} else if seeded {
		o.note("loop.seeded", map[string]any{"task": o.TaskID, "dir": o.Dir})
	}

	sum := &Summary{Status: StatusContinuing}
	prompt := KernelPrompt(o.Charter)
	resume := ""
	consecutiveErrors := 0

	for n := 1; o.MaxIterations < 0 || n <= o.MaxIterations; n++ {
		if err := ctx.Err(); err != nil {
			sum.Action, sum.Reason = Abort, "cancelled"
			return sum, err
		}

		started := time.Now()
		// Snapshot before dispatch: the worker rewrites the whole file, and
		// this is what makes losing an entry impossible rather than merely
		// discouraged.
		before, _ := Read(o.Dir)
		if o.Recall != nil {
			cur := Iteration{N: n, Charter: o.Charter, LoopMD: before, Status: ParseStatus(before)}
			if recalled := strings.TrimSpace(o.Recall(ctx, cur)); recalled != "" {
				merged := setSection(before, RecalledSection, recalled)
				if werr := Write(o.Dir, merged); werr == nil {
					before = merged
					o.note("loop.recalled", map[string]any{
						"task": o.TaskID, "iteration": n, "bytes": len(recalled),
					})
				}
			}
		}
		res, session, err := o.Dispatch(ctx, Iteration{N: n, Charter: o.Charter}, prompt, resume)
		if session != "" {
			resume, sum.Session = session, session
		}
		if res != nil {
			sum.SpentUSD += res.TotalCostUSD
		}
		sum.Iterations = n

		md, readErr := Read(o.Dir)
		if readErr != nil && err == nil {
			err = readErr
		}
		if merged, restored := MergePreserving(before, md); len(restored) > 0 {
			if werr := Write(o.Dir, merged); werr != nil {
				o.note("loop.memory_restore_failed", map[string]any{
					"task": o.TaskID, "iteration": n, "error": werr.Error(),
				})
			} else {
				md = merged
				o.note("loop.memory_restored", map[string]any{
					"task": o.TaskID, "iteration": n, "entries": len(restored),
					"first": firstLine(restored[0]),
				})
			}
		}
		// A turn the harness itself marked failed (its own budget cap, an API
		// error it swallowed) is an error for the loop's purposes too.
		// Otherwise a per-task budget set too low grinds silently to the
		// iteration ceiling instead of stopping after three.
		if err == nil && res != nil && res.IsError {
			err = fmt.Errorf("iteration %d: the worker reported failure: %s", n, firstLine(res.Result))
		}
		it := Iteration{
			N:        n,
			Charter:  o.Charter,
			LoopMD:   md,
			Status:   ParseStatus(md),
			Result:   res,
			Err:      err,
			SpentUSD: sum.SpentUSD,
			Elapsed:  time.Since(started),
		}
		sum.Status = it.Status

		// A worker that keeps failing is a broken setup, not a hard task.
		if err != nil {
			consecutiveErrors++
			// Escalate the recovery rather than repeating it. The first retry
			// keeps the session, since most failures are transient and the
			// transcript is worth having. A second consecutive failure drops
			// it and starts cold, because the likeliest cause of a repeated
			// failure is the transcript itself: it is never trimmed on this
			// path and `claude -p --resume` rebuilds the whole log into one
			// request, so a long run eventually cannot resume at all. Starting
			// cold is exactly what loop.md is for.
			if consecutiveErrors >= 2 && resume != "" {
				o.note("loop.cold_restart", map[string]any{
					"task": o.TaskID, "iteration": n, "dropped_session": resume,
					"reason": "consecutive failures; continuing from loop.md alone",
				})
				resume = ""
			}
			if consecutiveErrors >= o.MaxConsecutiveErrors {
				sum.Action = Abort
				sum.Reason = fmt.Sprintf("%d iterations in a row failed; last: %v", consecutiveErrors, err)
				o.report(it, Decision{Action: Abort, Reason: sum.Reason})
				return sum, err
			}
		} else {
			consecutiveErrors = 0
		}

		// Budget is the loop's to enforce, not the judge's: a judge that
		// wanted to keep going must not be able to overspend.
		if o.BudgetUSD > 0 && sum.SpentUSD >= o.BudgetUSD {
			sum.Action = Escalate
			sum.Reason = fmt.Sprintf("spent %.2f of a %.2f budget", sum.SpentUSD, o.BudgetUSD)
			o.report(it, Decision{Action: Escalate, Reason: sum.Reason})
			return sum, nil
		}

		d, jerr := o.Judge.Decide(ctx, it)
		if jerr != nil {
			sum.Action, sum.Reason = Escalate, "the judge failed: "+jerr.Error()
			o.report(it, Decision{Action: Escalate, Reason: sum.Reason})
			return sum, jerr
		}
		o.report(it, d)
		if o.Signal != nil {
			o.Signal(ctx, it, d)
		}

		if d.Action != Continue {
			sum.Action, sum.Reason = d.Action, d.Reason
			return sum, nil
		}
		prompt = d.Prompt
		if strings.TrimSpace(prompt) == "" {
			prompt = KernelPrompt(o.Charter)
		}
	}

	sum.Action = Escalate
	sum.Reason = fmt.Sprintf("hit the %d iteration ceiling still %s", o.MaxIterations, sum.Status)
	return sum, nil
}

func (o Options) report(it Iteration, d Decision) {
	if o.OnIteration != nil {
		o.OnIteration(it, d)
	}
	fields := map[string]any{
		"task":      o.TaskID,
		"iteration": it.N,
		"status":    it.Status,
		"action":    string(d.Action),
		"reason":    d.Reason,
		"spent_usd": it.SpentUSD,
		"elapsed_s": it.Elapsed.Seconds(),
	}
	if it.Err != nil {
		fields["error"] = it.Err.Error()
	}
	o.note("loop.iteration", fields)
}

func (o Options) note(kind string, v any) {
	if o.Ledger != nil {
		_ = o.Ledger.Note(kind, v)
	}
}

// KernelPrompt is the instruction every iteration starts from. It stays small
// and identical across iterations on purpose: loop.md carries the state, so
// the prompt does not have to, and the worker cannot drift by being told a
// slightly different thing each time.
func KernelPrompt(charter string) string {
	return fmt.Sprintf(`Read %s in the working directory. It is your memory across iterations: the charter, the plan, what has already been tried, and what has been learned. Earlier iterations wrote it; you are continuing their work, not starting over.

Do the next useful piece of work toward the charter.

Before you finish this turn, rewrite %s so the next iteration can pick up cold:
- Set the line under "## Status" to exactly one of: %s, %s, %s.
  Use %s only when the charter is genuinely satisfied, and %s when you need a
  human decision you cannot make yourself.
- Keep "## Plan" current, checking off what is finished.
- Append to "## Tried" what you attempted this turn and what happened, including
  what did not work. That is what stops the next iteration repeating it.
- Put durable facts about this codebase under "## Notes".

Charter: %s`,
		FileName, FileName,
		StatusContinuing, StatusDone, StatusBlocked,
		StatusDone, StatusBlocked,
		strings.TrimSpace(charter))
}

// firstLine trims a result to something that fits one ledger line.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "(no detail)"
	}
	return s
}
