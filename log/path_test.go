package log

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStabdardizePath(t *testing.T) {
	root := "/tmp/"
	serviceName := "mockSrv"
	ipv4 := `(([0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])\.){3}([0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])`
	reg := regexp.MustCompile(fmt.Sprintf(`\/tmp\/%s\/mockSrv.log`, ipv4))
	assert.Regexp(t, reg, StandardizePath(root, serviceName))
}

func TestStandardizePath_EmptyRoot(t *testing.T) {
	// An empty root must return "" so that log.Init skips file logging
	// and falls back to stderr. This is the intended behaviour when
	// RootDir is not set in the [Log] config section.
	assert.Equal(t, "", StandardizePath("", "mockSrv"))
}
