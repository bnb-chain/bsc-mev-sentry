package syncutils_test

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/bnb-chain/bsc-mev-sentry/syncutils"
)

func TestBatchRun(t *testing.T) {
	br := syncutils.NewBatchRunner()
	var sum1, sum2 int32
	for i := int32(1); i < 100000000; i++ {
		i := i
		br.AddTasks(func() error {
			atomic.AddInt32(&sum1, i)
			return nil
		})
		sum2 += i
	}
	err := br.WithConcurrencyLimit(30).Exec()
	assert.Nil(t, err)
	assert.Equal(t, sum2, sum1)

	err = syncutils.BatchRun(
		func() error {
			return nil
		},
		func() error {
			return errors.New("test_err")
		},
	)

	assert.EqualError(t, err, "test_err")

	err = syncutils.BatchRun(
		func() error {
			return errors.New("test_err1")
		},
		func() error {
			return errors.New("test_err2")
		},
	)

	assert.EqualError(t, err, "test_err1; test_err2")
}
