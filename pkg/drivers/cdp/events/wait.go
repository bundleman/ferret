package events

import (
	"context"
	"time"

	"github.com/MontFerret/ferret/pkg/drivers/cdp/eval"
	"github.com/MontFerret/ferret/pkg/runtime/core"
	"github.com/MontFerret/ferret/pkg/runtime/values"
)

type (
	Function func(ctx context.Context) (core.Value, error)

	WaitTask struct {
		fun     Function
		polling time.Duration
	}
)

const DefaultPolling = time.Millisecond * time.Duration(200)

func NewWaitTask(
	fun Function,
	polling time.Duration,
) *WaitTask {
	return &WaitTask{
		fun,
		polling,
	}
}

func (task *WaitTask) Run(ctx context.Context) (core.Value, error) {
	for {
		if ctx.Err() != nil {
			return values.None, ctx.Err()
		}

		out, err := task.fun(ctx)

		// expression failed
		// terminating
		if err != nil {
			return values.None, err
		}

		// output is not empty
		// terminating
		if out != values.None {
			return out, nil
		}

		// Nothing yet, let's wait before the next try
		<-time.After(task.polling)
	}
}

func NewEvalWaitTask(
	ec *eval.Runtime,
	fn *eval.Function,
	polling time.Duration,
) *WaitTask {
	return NewWaitTask(
		func(ctx context.Context) (core.Value, error) {
			return ec.EvalValue(ctx, fn)
		},
		polling,
	)
}

// NewEvalWaitTaskBuilder rebuilds the Function on every attempt; on a stale
// error it refreshes the isolated world so build() picks up a fresh id.
func NewEvalWaitTaskBuilder(
	ec *eval.Runtime,
	build func() *eval.Function,
	polling time.Duration,
) *WaitTask {
	return NewWaitTask(
		func(ctx context.Context) (core.Value, error) {
			out, err := ec.EvalValue(ctx, build())

			if err != nil && eval.IsStaleErr(err) {
				if refreshErr := ec.RefreshContext(ctx); refreshErr == nil {
					return ec.EvalValue(ctx, build())
				}
			}

			return out, err
		},
		polling,
	)
}
