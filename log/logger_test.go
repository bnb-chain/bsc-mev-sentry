package log_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/bnb-chain/bsc-mev-sentry/log"
	"github.com/bnb-chain/bsc-mev-sentry/log/internal/types"
)

func initTestLogger(lvl types.Level) {
	log.Init(lvl, "./tmp/test.log")
}

func printLogContent(t *testing.T) {
	log.Stop()

	files, _ := os.ReadDir("./tmp")
	for _, f := range files {
		fn := f.Name()
		if strings.HasPrefix(fn, "test") {
			t.Log(fn)
			content, _ := os.ReadFile("./tmp/" + fn)
			t.Log(string(content))
		}
	}
	os.RemoveAll("./tmp")
}

func testContext() context.Context {
	return context.TODO()
}

func Test_Log(t *testing.T) {
	mlog := log.With("m", "test")
	initTestLogger(types.DebugLevel)

	log.Debug("debug")
	log.Info("info")
	log.Warn("warn")
	log.Error("error")

	mlog.Info("info")
	//log.Panic("panic")

	printLogContent(t)
}

func Test_Logf(t *testing.T) {
	initTestLogger(types.DebugLevel)

	log.Debugf("msg: %v", "debug")
	log.Infof("msg: %v", "info")
	log.Warnf("msg: %v", "warn")
	log.Errorf("msg: %v", "error")
	//log.Panic("panic")

	printLogContent(t)
}

func Test_Logw(t *testing.T) {
	initTestLogger(types.DebugLevel)

	log.Debugw("msg: %v", "debug", 1)
	log.Infow("msg: %v", "info", 2)
	log.Warnw("msg: %v", "warn", 3)
	log.Errorw("msg: %v", "error", 4)
	//log.Panicw("panic", "panic", 4)

	printLogContent(t)
}

func Test_CtxLogf(t *testing.T) {
	initTestLogger(types.DebugLevel)

	log.CtxDebugf(testContext(), "msg: %v", "debug")
	log.CtxInfof(testContext(), "msg: %v", "info")
	log.CtxWarnf(testContext(), "msg: %v", "warn")
	log.CtxErrorf(testContext(), "msg: %v", "error")
	//log.Panic("panic")

	printLogContent(t)
}

func Test_CtxLogw(t *testing.T) {
	initTestLogger(types.DebugLevel)

	log.CtxDebugw(testContext(), "msg", "debug", 1, "ignore")
	log.CtxInfow(testContext(), "msg", "info", 1, 2, 3, 4)
	log.CtxWarnw(testContext(), "msg", "warn", 3)
	log.CtxErrorw(testContext(), "msg", "error", 4)
	//log.Panicw("panic", "panic", 4)

	printLogContent(t)
}

func Test_With(t *testing.T) {
	initTestLogger(types.DebugLevel)

	log.With("debug", nil, "ignore").Debug("test")
	log.With("info", nil, "info", 9).Info("test")
	log.With("warn", nil, 1, 2, 3, 4).Warn("test")
	log.With("error", nil, "key", "value").Error("test")

	log.With("t", 1, "hhh", "xxx", "hhh", "www").Warn("test")

	printLogContent(t)
}

// Verify that calling Init with an
// empty path does not create any files or directories.
// The logger falls back to stderr instead of creating a directory.
// This behavior lets the process supervisor (e.g. journald) handle log capture.
func Test_InitWithEmptyPathFallsBackToStderr(t *testing.T) {
	entriesBefore, err := os.ReadDir(".")
	assert.NoError(t, err)

	log.Init(types.InfoLevel, "")
	log.Info("fallback to stderr")

	entriesAfter, err := os.ReadDir(".")
	assert.NoError(t, err)
	assert.Equal(t, len(entriesBefore), len(entriesAfter),
		"Init with empty path must not create any files or directories")
}
