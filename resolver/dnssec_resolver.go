package resolver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/mia0x75/denyzer/cache"
	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/model"
	"github.com/mia0x75/denyzer/resolver/dnssec"
	"github.com/mia0x75/denyzer/util"
	"github.com/miekg/dns"
)

const ednsUDPSize = 4096

type DNSSECResolver struct {
	configurable[*config.DNSSEC]
	NextResolver
	typed

	validator    *dnssec.Validator
	upstreams    *UpstreamTreeResolver
	failureCache interface {
		cache.ExpiringCache[validationFailure]
		Del(key string)
	}
}

func NewDNSSECResolver(ctx context.Context, cfg config.DNSSEC, upstream Resolver) (ChainedResolver, error) {
	r := &DNSSECResolver{configurable: withConfig(&cfg), typed: withType("dnssec")}
	r.upstreams, _ = upstream.(*UpstreamTreeResolver)
	if cfg.IsEnabled() {
		anchors, err := dnssec.NewTrustAnchorStore(cfg.TrustAnchors)
		if err != nil {
			return nil, fmt.Errorf("load DNSSEC trust anchors: %w", err)
		}
		_, logger := r.log(ctx)
		r.validator = dnssec.NewValidator(ctx, anchors, logger, upstream, cfg.CacheExpirationHours,
			cfg.MaxChainDepth, cfg.MaxNSEC3Iterations, cfg.MaxUpstreamQueries, cfg.ClockSkewToleranceSec)
		failures := cache.NewCache[validationFailure](cache.Options{
			CleanupInterval: dnssec.BogusTTL, MaxSize: failureCacheSize,
		})
		failures.Start(ctx)
		r.failureCache = failures
		logger.InfoContext(ctx, "DNSSEC resolver initialized", "trustAnchors", len(anchors.GetRootTrustAnchors()))
	}

	return r, nil
}

func (r *DNSSECResolver) Resolve(ctx context.Context, request *model.Request) (*model.Response, error) {
	if !r.cfg.Validate || len(request.Req.Question) == 0 {
		return r.next.Resolve(ctx, request)
	}
	ctx, logger := r.log(ctx)
	// The server restores the client's DO preference after local validation.
	if opt := request.Req.IsEdns0(); opt != nil {
		opt.SetDo(true)
		opt.SetUDPSize(max(opt.UDPSize(), ednsUDPSize))
	} else {
		request.Req.SetEdns0(ednsUDPSize, true)
	}
	q := &askedQuery{
		view:   r.upstreamGroup(ctx, logger, request),
		subnet: util.GetEdns0Option[*dns.EDNS0_SUBNET](request.Req),
	}

	return r.answer(ctx, logger, request, q)
}

func (r *DNSSECResolver) answer(
	ctx context.Context, logger *slog.Logger, request *model.Request, q *askedQuery,
) (*model.Response, error) {
	if r.validator != nil {
		if failure := r.refused(request, q); failure != nil {
			return failure.response(request), nil
		}
	}
	response, err := r.next.Resolve(ctx, request)
	if err != nil {
		return nil, err
	}
	if response == nil || response.Res == nil {
		return nil, errors.New("DNSSEC: resolver returned no response")
	}
	if r.validator == nil {
		return response, nil
	}
	// Upstream coalescing may share the response with concurrent clients.
	owned := *response
	owned.Res = response.Res.Copy()
	response = &owned
	if reason := skipValidationReason(request, response); reason != "" {
		response.Res.AuthenticatedData = false
		logger.DebugContext(ctx, "skipping DNSSEC validation", "reason", reason)

		return response, nil
	}
	validationCtx := dnssec.WithClientContext(ctx, request.ClientIP, request.ClientNames, request.RequestClientID, q.view)
	if !request.AskedAgain {
		validationCtx = dnssec.FirstAsk(validationCtx)
	}
	verdict := r.validator.Validate(validationCtx, response.Res, request.Req.Question[0])
	logger.DebugContext(ctx, "DNSSEC validation result", "domain", request.Req.Question[0].Name,
		"result", verdict.Result.String())
	switch verdict.Result {
	case dnssec.ValidationResultBogus:
		return r.failed(validationCtx, logger, request, q, verdict)
	case dnssec.ValidationResultSecure:
		r.validated(q)
		markAuthentic(response.Res, verdict.Lifetimes)
	case dnssec.ValidationResultInsecure, dnssec.ValidationResultIndeterminate:
		r.validated(q)
		response.Res.AuthenticatedData = false
	}

	return response, nil
}

func markAuthentic(msg *dns.Msg, lifetimes dnssec.Lifetimes) {
	msg.AuthenticatedData = true
	msg.Answer = slices.DeleteFunc(slices.Clone(msg.Answer), dnssec.CoversNoRRset(msg.Answer))
	msg.Ns = slices.DeleteFunc(slices.Clone(msg.Ns), dnssec.CoversNoRRset(msg.Ns))
	msg.Ns = dnssec.ValidatedAuthority(msg)
	msg.Answer, msg.Ns = lifetimes.Bound(msg.Answer), lifetimes.Bound(msg.Ns)
	// AD covers only authenticated Answer and Authority data; OPT carries protocol metadata.
	msg.Extra = slices.DeleteFunc(slices.Clone(msg.Extra), func(rr dns.RR) bool {
		return rr.Header().Rrtype != dns.TypeOPT
	})
}

func skipValidationReason(request *model.Request, response *model.Response) string {
	if response == nil || response.Res == nil {
		return ""
	}
	// Locally assigned response types distinguish private or synthesized data from public answers.
	if response.RType != model.ResponseTypeRESOLVED && response.RType != model.ResponseTypeCACHED {
		return fmt.Sprintf("trusted-local/synthesized response (%s)", response.RType)
	}
	if request.Req.CheckingDisabled {
		return "query with CD bit set"
	}
	if answersRRSIGQuestion(request.Req.Question[0], response.Res) {
		return "answer to a query for RRSIG records"
	}

	return ""
}

func answersRRSIGQuestion(question dns.Question, response *dns.Msg) bool {
	return question.Qtype == dns.TypeRRSIG && response.Rcode == dns.RcodeSuccess && len(response.Answer) > 0
}

func (r *DNSSECResolver) upstreamGroup(ctx context.Context, logger *slog.Logger, request *model.Request) string {
	if r.upstreams == nil {
		return ""
	}

	return r.upstreams.upstreamGroupByClient(ctx, logger, request)
}

func createServFailResponseDNSSEC(request *model.Request, code uint16, reason string) *model.Response {
	response := model.NewResponseWithRcode(request, dns.RcodeServerFailure, model.ResponseTypeBOGUS, reason)
	response.Res.SetEdns0(ednsUDPSize, false)
	util.SetEdns0Option(response.Res, &dns.EDNS0_EDE{
		InfoCode: code, ExtraText: truncateText(reason, maxEDETextLength),
	})

	return response
}
