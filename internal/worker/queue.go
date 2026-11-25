package worker

import (
	"context"
	"errors"
)

type task struct {
	ctx    context.Context
	run    func(context.Context) (any, error)
	result chan outcome
}
type outcome struct {
	value any
	err   error
}
type queue struct {
	ctx  context.Context
	jobs chan task
}

func newQueue(ctx context.Context, limit int) *queue {
	q := &queue{ctx, make(chan task, limit)}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case job := <-q.jobs:
				if job.ctx.Err() != nil {
					job.result <- outcome{err: job.ctx.Err()}
					continue
				}
				value, err := job.run(job.ctx)
				job.result <- outcome{value, err}
			}
		}
	}()
	return q
}
func (q *queue) do(ctx context.Context, fn func(context.Context) (any, error)) (any, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(q.ctx, cancel)
	defer stop()
	job := task{ctx, fn, make(chan outcome, 1)}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case q.jobs <- job:
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, errors.New("Local inference queue is full")
	}
	select {
	case result := <-job.result:
		return result.value, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
