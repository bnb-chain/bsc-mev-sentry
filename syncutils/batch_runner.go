package syncutils

import (
	"sync"
	"sync/atomic"

	"go.uber.org/multierr"
)

type Task func() error

// BatchRunner is a tool to run tasks concurrently, its methods are not thread-safe
type BatchRunner struct {
	concurrencyLimit int

	tasks []Task
}

// NewBatchRunner creates BatchRunner
func NewBatchRunner() *BatchRunner {
	return &BatchRunner{}
}

// Reset resets all settings and clear tasks
func (br *BatchRunner) Reset() *BatchRunner {
	*br = BatchRunner{}
	return br
}

// WithConcurrencyLimit sets concurrency limit
func (br *BatchRunner) WithConcurrencyLimit(limit int) *BatchRunner {
	br.concurrencyLimit = limit
	return br
}

// AddTasks adds tasks
func (br *BatchRunner) AddTasks(task ...Task) *BatchRunner {
	br.tasks = append(br.tasks, task...)
	return br
}

// Exec execute all added tasks concurrently
func (br *BatchRunner) Exec() error {
	tasksCount := len(br.tasks)
	if tasksCount == 0 {
		return nil
	}
	if tasksCount == 1 {
		return br.tasks[0]()
	}

	concurLimit := br.concurrencyLimit
	if concurLimit == 0 || concurLimit > tasksCount {
		concurLimit = tasksCount
	}

	errs := make([]error, tasksCount)

	var wg sync.WaitGroup
	wg.Add(concurLimit)

	tidx := int32(-1)
	execFunc := func() {
		defer wg.Done()

		for {
			idx := atomic.AddInt32(&tidx, 1)
			if int(idx) >= tasksCount {
				return
			}

			errs[idx] = br.tasks[idx]()
		}
	}

	for i := 1; i < concurLimit; i++ {
		go execFunc()
	}
	execFunc()

	wg.Wait()
	return multierr.Combine(errs...)
}

// BatchRun runs tasks concurrently
func BatchRun(tasks ...Task) error {
	return NewBatchRunner().AddTasks(tasks...).Exec()
}
