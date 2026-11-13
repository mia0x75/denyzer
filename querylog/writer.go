package querylog

import "time"

type LogEntry struct {
	Start           time.Time
	ClientIP        string
	ClientNames     []string
	DurationMs      int64
	ResponseReason  string
	ResponseType    string
	ResponseCode    string
	QuestionType    string
	QuestionName    string
	Answer          string
	DenyzerInstance string
}

type Writer interface {
	Write(entry *LogEntry)
	// RecordError records a query that failed before a log entry could be
	// created (e.g. a resolver chain error). Writers that do not persist
	// statistics treat it as a no-op.
	RecordError()
	CleanUp()
}
