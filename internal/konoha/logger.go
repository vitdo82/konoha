package konoha

import (
	"context"
	stdlog "log"

	"go.temporal.io/sdk/activity"
	tlog "go.temporal.io/sdk/log"
)

// activityLogger returns the Temporal activity logger inside an activity, and a
// process logger outside one (#212). The local runtime invokes the same activity
// functions in-process, where activity.GetLogger panics ("Not an activity
// context"); this helper keeps that path safe without changing Temporal behavior.
func activityLogger(ctx context.Context) tlog.Logger {
	if activity.IsActivity(ctx) {
		return activityLogger(ctx)
	}
	return fallbackLogger{}
}

// fallbackLogger satisfies the Temporal log.Logger surface for in-process calls.
type fallbackLogger struct{}

func (fallbackLogger) Debug(string, ...interface{}) {}

func (fallbackLogger) Info(msg string, kv ...interface{}) { stdlog.Printf("INFO %s %v", msg, kv) }

func (fallbackLogger) Warn(msg string, kv ...interface{}) { stdlog.Printf("WARN %s %v", msg, kv) }

func (fallbackLogger) Error(msg string, kv ...interface{}) { stdlog.Printf("ERROR %s %v", msg, kv) }
