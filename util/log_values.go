package util

import (
	"log/slog"
	"slices"

	"github.com/miekg/dns"
)

// Obfuscated defers privacy filtering until an enabled record is emitted.
type Obfuscated string

func (s Obfuscated) LogValue() slog.Value { return slog.StringValue(Obfuscate(string(s))) }

// LogQuestions snapshots the original questions without formatting them. The
// single-question path owns the value directly, avoiding a separate slice copy.
func LogQuestions(questions []dns.Question) slog.LogValuer {
	if len(questions) == 1 {
		return questionLogValue(questions[0])
	}

	return questionsLogValue(slices.Clone(questions))
}

type questionLogValue dns.Question

func (q questionLogValue) LogValue() slog.Value {
	return slog.StringValue(QuestionToString([]dns.Question{dns.Question(q)}))
}

type questionsLogValue []dns.Question

func (q questionsLogValue) LogValue() slog.Value {
	return slog.StringValue(QuestionToString(q))
}

// LogAnswer snapshots mutable DNS records and defers formatting and privacy
// filtering. Guard its construction with Enabled when the level may be disabled.
func LogAnswer(answer []dns.RR) slog.LogValuer {
	copyOf := make(answerLogValue, len(answer))
	for i, rr := range answer {
		copyOf[i] = dns.Copy(rr)
	}

	return copyOf
}

type answerLogValue []dns.RR

func (a answerLogValue) LogValue() slog.Value {
	return slog.StringValue(Obfuscate(AnswerToString(a)))
}
