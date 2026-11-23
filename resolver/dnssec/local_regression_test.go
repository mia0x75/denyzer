package dnssec

import (
	"context"
	"errors"
	"net"

	"github.com/mia0x75/denyzer/log"
	"github.com/mia0x75/denyzer/model"
	"github.com/miekg/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Local alias validation regressions", func() {
	var ctx context.Context
	var mockUpstream *mockResolver
	var sut *Validator
	BeforeEach(func(specCtx SpecContext) {
		ctx = specCtx
		mockUpstream = &mockResolver{}
		logger, _ := log.NewMockLogger()
		anchors, err := NewTrustAnchorStore(nil)
		Expect(err).Should(Succeed())
		sut = NewValidator(ctx, anchors, logger, mockUpstream, 1, 10, 150, 100, 3600)
	})
	Context("with a signed cross-zone alias", func() {
		var (
			chain     *signedChain
			validator *Validator
			response  *dns.Msg
			question  dns.Question
		)
		BeforeEach(func() {
			chain = newSignedChain()
			mockUpstream.ResolveFn = chain.resolve
			validator = NewValidator(ctx, chain.anchors, sut.logger, mockUpstream, 1, 10, 150, 100, 3600)
			alias := &dns.CNAME{
				Hdr:    dns.RR_Header{Name: "alias.example.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 300},
				Target: "target.signed.example.",
			}
			question = dns.Question{Name: alias.Hdr.Name, Qtype: dns.TypeA, Qclass: dns.ClassINET}
			response = &dns.Msg{Rcode: dns.RcodeNameError, Answer: []dns.RR{
				alias, signRRset([]dns.RR{alias}, dns.TypeCNAME, chain.parentKey, chain.parentPriv, chain.parent),
			}}
		})

		It("authenticates NSEC3 denial at the target of an unordered multi-hop alias", func() {
			next := &dns.CNAME{
				Hdr:    dns.RR_Header{Name: "target.signed.example.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 300},
				Target: "missing.signed.example.",
			}
			response.Answer = append([]dns.RR{
				next,
				signRRset([]dns.RR{next}, dns.TypeCNAME, chain.childKey, chain.childPriv, chain.child),
			}, response.Answer...)
			hash := dns.HashName(chain.child, dns.SHA1, 0, "")
			nsec := &dns.NSEC3{
				Hdr:  dns.RR_Header{Name: hash + "." + chain.child, Rrtype: dns.TypeNSEC3, Class: dns.ClassINET, Ttl: 300},
				Hash: dns.SHA1, HashLength: 20, NextDomain: hash,
				TypeBitMap: []uint16{dns.TypeNS, dns.TypeSOA, dns.TypeRRSIG, dns.TypeDNSKEY},
			}
			response.Ns = []dns.RR{nsec, signRRset([]dns.RR{nsec}, dns.TypeNSEC3, chain.childKey, chain.childPriv, chain.child)}
			Expect(validator.ValidateResponse(ctx, response, question)).Should(Equal(ValidationResultSecure))
			next.Target = "outside.example."
			response.Answer[1] = signRRset([]dns.RR{next}, dns.TypeCNAME, chain.childKey, chain.childPriv, chain.child)
			Expect(validator.ValidateResponse(ctx, response, question)).Should(Equal(ValidationResultBogus))
		})

		It("rejects a forged NXDOMAIN even when the signed terminal address is present", func() {
			response.Answer = append(response.Answer,
				signedA("target.signed.example.", "192.0.2.1", chain.childKey, chain.childPriv, chain.child).Answer...)
			Expect(validator.ValidateResponse(ctx, response, question)).Should(Equal(ValidationResultBogus))
		})

		It("rejects an invalid alias signature despite a valid target proof", func() {
			nsec := &dns.NSEC{
				Hdr:        dns.RR_Header{Name: chain.child, Rrtype: dns.TypeNSEC, Class: dns.ClassINET, Ttl: 300},
				NextDomain: chain.child, TypeBitMap: []uint16{dns.TypeSOA, dns.TypeRRSIG, dns.TypeNSEC},
			}
			response.Ns = []dns.RR{nsec, signRRset([]dns.RR{nsec}, dns.TypeNSEC, chain.childKey, chain.childPriv, chain.child)}
			response.Answer[1].(*dns.RRSIG).Signature = "AAAA"
			Expect(validator.ValidateResponse(ctx, response, question)).Should(Equal(ValidationResultBogus))
		})

		It("requires a terminal proof for an address query but accepts a direct CNAME query", func() {
			response.Rcode = dns.RcodeSuccess
			Expect(validator.ValidateResponse(ctx, response, question)).Should(Equal(ValidationResultBogus))
			question.Qtype = dns.TypeCNAME
			Expect(validator.ValidateResponse(ctx, response, question)).Should(Equal(ValidationResultSecure))
		})

		It("rejects a signed SOA without the required denial proof", func() {
			soa := &dns.SOA{
				Hdr: dns.RR_Header{Name: chain.child, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 300},
				Ns:  "ns.signed.example.", Mbox: "hostmaster.signed.example.",
			}
			response.Rcode = dns.RcodeSuccess
			response.Ns = []dns.RR{soa, signRRset([]dns.RR{soa}, dns.TypeSOA, chain.childKey, chain.childPriv, chain.child)}
			Expect(validator.ValidateResponse(ctx, response, question)).Should(Equal(ValidationResultBogus))
		})

		It("preserves Indeterminate when the target security status cannot be retrieved", func() {
			alias := response.Answer[0].(*dns.CNAME)
			alias.Target = "target.other."
			response.Answer[1] = signRRset([]dns.RR{alias}, dns.TypeCNAME, chain.parentKey, chain.parentPriv, chain.parent)
			mockUpstream.ResolveFn = func(ctx context.Context, req *model.Request) (*model.Response, error) {
				if req.Req.Question[0].Qtype == dns.TypeDS {
					return nil, errors.New("MOCK upstream unavailable")
				}

				return chain.resolve(ctx, req)
			}
			Expect(validator.ValidateResponse(ctx, response, question)).Should(Equal(ValidationResultIndeterminate))
		})
	})

	DescribeTable("authenticates the terminal conclusion independently of the alias",
		func(rcode int, proofName string, want ValidationResult) {
			chain := newSignedChain()
			mockUpstream.ResolveFn = chain.resolve
			validator := NewValidator(ctx, chain.anchors, sut.logger, mockUpstream, 1, 10, 150, 100, 3600)
			alias := &dns.CNAME{
				Hdr:    dns.RR_Header{Name: "alias.example.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 300},
				Target: "target.signed.example.",
			}
			response := &dns.Msg{Rcode: rcode, Answer: []dns.RR{
				alias, signRRset([]dns.RR{alias}, dns.TypeCNAME, chain.parentKey, chain.parentPriv, chain.parent),
			}}
			if proofName != "" {
				nsec := &dns.NSEC{
					Hdr:        dns.RR_Header{Name: proofName, Rrtype: dns.TypeNSEC, Class: dns.ClassINET, Ttl: 300},
					NextDomain: chain.child, TypeBitMap: []uint16{dns.TypeAAAA, dns.TypeRRSIG, dns.TypeNSEC},
				}
				response.Ns = []dns.RR{nsec, signRRset([]dns.RR{nsec}, dns.TypeNSEC, chain.childKey, chain.childPriv, chain.child)}
			}
			question := dns.Question{Name: alias.Hdr.Name, Qtype: dns.TypeA, Qclass: dns.ClassINET}
			Expect(validator.ValidateResponse(ctx, response, question)).Should(Equal(want))
		},
		Entry("rejects forged NXDOMAIN without proof", dns.RcodeNameError, "", ValidationResultBogus),
		Entry("accepts NXDOMAIN proven for the target", dns.RcodeNameError, "signed.example.", ValidationResultSecure),
		Entry("accepts NODATA proven for the target", dns.RcodeSuccess, "target.signed.example.", ValidationResultSecure),
		Entry("rejects NODATA proven for another name", dns.RcodeSuccess, "other.signed.example.", ValidationResultBogus),
	)

	It("does not authenticate an unsigned terminal address using a signed alias", func() {
		chain := newSignedChain()
		anchor, resolveUnsigned := authenticatedInsecureDelegation("net.", "unsigned.net.")
		store, err := NewTrustAnchorStore([]string{chain.parentKey.String(), anchor})
		Expect(err).Should(Succeed())
		mockUpstream.ResolveFn = func(ctx context.Context, req *model.Request) (*model.Response, error) {
			if dns.IsSubDomain("net.", req.Req.Question[0].Name) {
				return resolveUnsigned(ctx, req)
			}

			return chain.resolve(ctx, req)
		}
		validator := NewValidator(ctx, store, sut.logger, mockUpstream, 1, 10, 150, 100, 3600)
		alias := &dns.CNAME{
			Hdr:    dns.RR_Header{Name: "alias.example.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 300},
			Target: "unsigned.net.",
		}
		address := &dns.A{Hdr: dns.RR_Header{Name: alias.Target, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300}, A: net.IPv4(192, 0, 2, 1)}
		response := &dns.Msg{Answer: []dns.RR{
			alias, signRRset([]dns.RR{alias}, dns.TypeCNAME, chain.parentKey, chain.parentPriv, chain.parent), address,
		}}
		Expect(validator.ValidateResponse(ctx, response, dns.Question{
			Name: alias.Hdr.Name, Qtype: dns.TypeA, Qclass: dns.ClassINET,
		})).Should(Equal(ValidationResultInsecure))
		response.Answer = response.Answer[:2]
		response.Rcode = dns.RcodeNameError
		Expect(validator.ValidateResponse(ctx, response, dns.Question{
			Name: alias.Hdr.Name, Qtype: dns.TypeA, Qclass: dns.ClassINET,
		})).Should(Equal(ValidationResultInsecure))
	})
})
