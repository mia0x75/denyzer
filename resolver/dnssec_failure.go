package resolver

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"time"

	"github.com/mia0x75/denyzer/model"
	"github.com/mia0x75/denyzer/resolver/dnssec"
	"github.com/mia0x75/denyzer/util"
	"github.com/miekg/dns"
)

const (
	failureCacheSize = 1000
	firstRefusal     = time.Second
	failureMemory    = dnssec.BogusTTL
)

type validationFailure struct {
	reason  string
	refused time.Duration
	code    uint16
}

type askedQuery struct {
	subnet *dns.EDNS0_SUBNET
	last   *validationFailure
	view   string
	key    string
}

func (q *askedQuery) failureKey(request *model.Request) string {
	if q.key == "" {
		q.key = failureCacheKey(request, q.view, q.subnet)
	}

	return q.key
}

func failureCacheKey(request *model.Request, view string, subnet *dns.EDNS0_SUBNET) string {
	query := request.Req.Copy()
	query.Id = 0
	for i := range query.Question {
		query.Question[i].Name = dns.CanonicalName(query.Question[i].Name)
	}
	// ECS may be rewritten below this resolver; preserve the client's original option separately.
	util.RemoveEdns0OptionKeepRecord[*dns.EDNS0_SUBNET](query)
	packed, err := query.Pack()
	if err != nil {
		return ""
	}
	named := append([]byte(view+"\x00"), packed...)
	if subnet != nil {
		named = fmt.Appendf(named, "\x00%d %d %d %s",
			subnet.Family, subnet.SourceNetmask, subnet.SourceScope, subnet.Address)
	}
	digest := sha256.Sum256(named)

	return string(digest[:])
}

func (f *validationFailure) response(request *model.Request) *model.Response {
	return createServFailResponseDNSSEC(request, f.code, f.reason)
}

func (r *DNSSECResolver) refused(request *model.Request, q *askedQuery) *validationFailure {
	q.last = nil
	if r.failureCache.TotalCount() == 0 {
		return nil
	}
	key := q.failureKey(request)
	if key == "" {
		return nil
	}
	last, left := r.failureCache.Get(key)
	if last == nil || left <= 0 {
		return nil
	}
	q.last = last
	if left <= failureMemory {
		return nil
	}

	return last
}

func refusalAfter(last *validationFailure) time.Duration {
	if last == nil {
		return firstRefusal
	}

	return min(2*last.refused, dnssec.BogusTTL)
}

func (r *DNSSECResolver) validated(q *askedQuery) {
	if q.last != nil {
		r.failureCache.Del(q.key)
	}
}

func (r *DNSSECResolver) failed(
	ctx context.Context, logger *slog.Logger, request *model.Request, q *askedQuery, verdict dnssec.Verdict,
) (*model.Response, error) {
	failure := &validationFailure{code: verdict.EDE, reason: "DNSSEC validation failed: " + verdict.Text}
	// A second validation shares the first one's budget and cannot repair an unanswered lookup.
	if !request.AskedAgain && !verdict.RanOut && !verdict.NoReply {
		again := *request
		again.Req = request.Req.Copy()
		again.AskedAgain = true
		response, err := r.answer(dnssec.Again(ctx), logger, &again, q)
		if err == nil {
			return response, nil
		}
		logger.DebugContext(ctx, "no second DNSSEC reply", "error", err)
	}
	// Exhaustion describes this reply's cost, so it must not poison later queries.
	if !verdict.RanOut {
		if key := q.failureKey(request); key != "" {
			failure.refused = refusalAfter(q.last)
			r.failureCache.Put(key, failure, failure.refused+failureMemory)
		}
	}
	logger.WarnContext(ctx, "DNSSEC validation failed", "domain", request.Req.Question[0].Name,
		"reason", verdict.Text)

	return failure.response(request), nil
}
