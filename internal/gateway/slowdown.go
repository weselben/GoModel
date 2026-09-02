package gateway

import (
	"context"
	"time"

	"github.com/enterpilot/gomodel/internal/core"
)

func workflowSlowdown(workflow *core.Workflow) float64 {
	if workflow == nil || workflow.Resolution == nil || workflow.Resolution.Slowdown <= 0 {
		return 0
	}
	return workflow.Resolution.Slowdown
}

// workflowRepetition extracts per-request repetition-guard overrides from the
// matched workflow. ok reports whether at least one field carries an explicit
// value: when false, the orchestrator globals should be used instead.
// Per-model 0 with ok=true means "guard OFF" for the limit (a hard disable);
// 0 with ok=false means "inherit the global" (the field is nil).
func workflowRepetition(workflow *core.Workflow) (limit, maxPattern int, ok bool) {
	if workflow == nil || workflow.Resolution == nil {
		return 0, 0, false
	}
	res := workflow.Resolution
	limitSet := res.RepetitionLimit != nil
	maxSet := res.RepetitionMaxPattern != nil
	if !limitSet && !maxSet {
		return 0, 0, false
	}
	if limitSet {
		limit = *res.RepetitionLimit
	}
	if maxSet {
		maxPattern = *res.RepetitionMaxPattern
	}
	return limit, maxPattern, true
}

func waitForInferenceSlowdown(ctx context.Context, workflow *core.Workflow, inferenceTime time.Duration) error {
	factor := workflowSlowdown(workflow)
	if factor <= 0 || inferenceTime <= 0 {
		return nil
	}
	delay := time.Duration(float64(inferenceTime) * factor)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func executeResultWithSlowdown[Result any](
	ctx context.Context,
	workflow *core.Workflow,
	execute func() (Result, error),
) (Result, error) {
	started := time.Now()
	result, err := execute()
	if err != nil {
		var zero Result
		return zero, err
	}
	if err := waitForInferenceSlowdown(ctx, workflow, time.Since(started)); err != nil {
		var zero Result
		return zero, err
	}
	return result, nil
}

func dispatchTranslatedWithSlowdown[Response any](
	ctx context.Context,
	workflow *core.Workflow,
	execute func() (Response, ExecutionMeta, error),
) (Response, ExecutionMeta, error) {
	started := time.Now()
	resp, meta, err := execute()
	if err != nil {
		var zero Response
		return zero, ExecutionMeta{}, err
	}
	if err := waitForInferenceSlowdown(ctx, workflow, time.Since(started)); err != nil {
		var zero Response
		return zero, ExecutionMeta{}, err
	}
	return resp, meta, nil
}
