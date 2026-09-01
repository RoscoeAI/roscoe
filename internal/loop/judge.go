package loop

import (
	"context"
	"fmt"
)

// StatusJudge takes the worker at its word: it reads the status the worker
// wrote in loop.md and nothing else. It is the deterministic default, and the
// floor the quorum has to beat. It deliberately holds no opinion about whether
// the work is actually good; that judgment is what the quorum adds.
type StatusJudge struct{}

func (StatusJudge) Decide(_ context.Context, it Iteration) (Decision, error) {
	switch {
	case it.Err != nil:
		// One failure is worth retrying: transient API errors are common and
		// the transcript survives. The loop stops a run that keeps failing.
		return Decision{
			Action: Continue,
			Prompt: fmt.Sprintf("The previous iteration failed with: %v\n\n%s",
				it.Err, KernelPrompt(it.Charter, Projection(it.LoopMD, 0))),
			Reason:     "retrying after a failed iteration",
			Confidence: 0.5,
		}, nil

	case it.Status == StatusDone:
		return Decision{Action: Done, Reason: "the worker reported done", Confidence: 1}, nil

	case it.Status == StatusBlocked:
		return Decision{Action: Escalate, Reason: "the worker reported blocked", Confidence: 1}, nil

	default:
		return Decision{
			Action:     Continue,
			Prompt:     KernelPrompt(it.Charter, Projection(it.LoopMD, 0)),
			Reason:     "still continuing",
			Confidence: 1,
		}, nil
	}
}

// FixedJudge always returns the same decision. It exists for tests and for
// `--once`, which runs a single iteration and stops.
type FixedJudge struct{ D Decision }

func (f FixedJudge) Decide(context.Context, Iteration) (Decision, error) { return f.D, nil }
