package util

import (
	"net"
	"testing"

	"github.com/mia0x75/denyzer/log"
	"github.com/miekg/dns"
)

func TestObfuscatedLogValue(t *testing.T) {
	previous := LogPrivacy.Load()
	t.Cleanup(func() { LogPrivacy.Store(previous) })
	value := Obfuscated("Client12.example.com.")
	for _, privacy := range []bool{false, true, false} {
		LogPrivacy.Store(privacy)
		if got, want := value.LogValue().String(), Obfuscate(string(value)); got != want {
			t.Fatalf("privacy %t: value = %q, want %q", privacy, got, want)
		}
	}
}

func TestLogQuestionsSnapshot(t *testing.T) {
	previous := LogPrivacy.Load()
	t.Cleanup(func() { LogPrivacy.Store(previous) })
	tests := []struct {
		name      string
		questions []dns.Question
	}{
		{name: "empty"},
		{
			name: "single",
			questions: []dns.Question{
				{Name: "original.example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
			},
		},
		{
			name: "multiple",
			questions: []dns.Question{
				{Name: "original.example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
				{Name: "second.example.com.", Qtype: dns.TypeAAAA, Qclass: dns.ClassINET},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			LogPrivacy.Store(false)
			want := QuestionToString(tt.questions)
			value := LogQuestions(tt.questions)
			for i := range tt.questions {
				tt.questions[i].Name = "rewritten.example.net."
				tt.questions[i].Qtype = dns.TypeMX
			}
			if got := value.LogValue().String(); got != want {
				t.Fatalf("question snapshot = %q, want %q", got, want)
			}
			LogPrivacy.Store(true)
			if got := value.LogValue().String(); got != Obfuscate(want) {
				t.Fatalf("private question snapshot = %q, want %q", got, Obfuscate(want))
			}
		})
	}
}

func TestLogAnswerSnapshot(t *testing.T) {
	previous := LogPrivacy.Load()
	t.Cleanup(func() { LogPrivacy.Store(previous) })
	LogPrivacy.Store(false)
	address := &dns.A{A: net.ParseIP("192.0.2.1")}
	alias := &dns.CNAME{Target: "original.example.com."}
	text := &dns.TXT{
		Hdr: dns.RR_Header{Name: "original.example.com.", Rrtype: dns.TypeTXT, Class: dns.ClassINET},
		Txt: []string{"original"},
	}
	answer := []dns.RR{address, alias, text}
	want := AnswerToString(answer)
	value := LogAnswer(answer)
	address.A[len(address.A)-1] = 99
	alias.Target = "changed.example.net."
	text.Txt[0] = "changed"
	text.Hdr.Ttl = 999
	answer[0] = &dns.AAAA{AAAA: net.ParseIP("2001:db8::1")}
	if got := value.LogValue().String(); got != want {
		t.Fatalf("answer snapshot = %q, want %q", got, want)
	}
	LogPrivacy.Store(true)
	if got := value.LogValue().String(); got != Obfuscate(want) {
		t.Fatalf("private answer snapshot = %q, want %q", got, Obfuscate(want))
	}
	if got := LogAnswer(nil).LogValue().String(); got != "" {
		t.Fatalf("empty answer = %q", got)
	}
}

func TestLogValuesBufferedSnapshot(t *testing.T) {
	previous := LogPrivacy.Load()
	t.Cleanup(func() { LogPrivacy.Store(previous) })
	LogPrivacy.Store(true)
	questions := []dns.Question{{Name: "original.example.com.", Qtype: dns.TypeA}}
	answer := []dns.RR{&dns.CNAME{Target: "original.example.com."}}
	wantQuestion, wantAnswer := QuestionToString(questions), Obfuscate(AnswerToString(answer))
	buffer := log.NewBuffer()
	buffer.Logger().Info("buffered", "question", LogQuestions(questions), "answer", LogAnswer(answer))
	questions[0].Name = "changed.example.net."
	answer[0].(*dns.CNAME).Target = "changed.example.net."
	LogPrivacy.Store(false)
	logger, recorder := log.NewMockLogger()
	buffer.Replay(logger)
	fields := log.RecordFields(recorder.LastRecord())
	if fields["question"] != wantQuestion || fields["answer"] != wantAnswer {
		t.Fatalf("buffer changed the captured private values: %#v", fields)
	}
}
