package resolver

import (
	"context"
	"crypto"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/mia0x75/denyzer/config"
	. "github.com/mia0x75/denyzer/helpertest"
	"github.com/mia0x75/denyzer/log"
	"github.com/mia0x75/denyzer/model"
	"github.com/mia0x75/denyzer/resolver/dnssec"
	"github.com/mia0x75/denyzer/util"
	"github.com/miekg/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
)

var _ = Describe("DNSSECResolver", func() {
	var (
		sut          *DNSSECResolver
		sutConfig    config.DNSSEC
		mockUpstream *mockResolver
		ctx          context.Context
	)

	Describe("Type", func() {
		It("follows conventions", func() {
			ctx := context.Background()
			mockUpstream := &mockResolver{}

			sut, _ := NewDNSSECResolver(ctx, config.DNSSEC{Validate: false}, mockUpstream)
			expectValidResolverType(sut)
		})
	})

	BeforeEach(func() {
		ctx = context.Background()
		sutConfig = config.DNSSEC{
			Validate: false,
		}
		mockUpstream = &mockResolver{}
	})

	Describe("NewDNSSECResolver", func() {
		When("DNSSEC validation is disabled", func() {
			It("should create resolver without validator", func() {
				resolver, err := NewDNSSECResolver(ctx, sutConfig, mockUpstream)
				Expect(err).Should(Succeed())
				Expect(resolver).ShouldNot(BeNil())

				// Type assert to access validator field
				dnssecResolver, ok := resolver.(*DNSSECResolver)
				Expect(ok).Should(BeTrue())
				Expect(dnssecResolver.validator).Should(BeNil())
			})
		})

		When("DNSSEC validation is enabled", func() {
			BeforeEach(func() {
				sutConfig.Validate = true
			})

			It("should create resolver with validator", func() {
				resolver, err := NewDNSSECResolver(ctx, sutConfig, mockUpstream)
				Expect(err).Should(Succeed())
				Expect(resolver).ShouldNot(BeNil())

				// Type assert to access validator field
				dnssecResolver, ok := resolver.(*DNSSECResolver)
				Expect(ok).Should(BeTrue())
				Expect(dnssecResolver.validator).ShouldNot(BeNil())
			})
		})

		When("custom trust anchors are provided", func() {
			BeforeEach(func() {
				sutConfig.Validate = true
				sutConfig.TrustAnchors = []string{
					". 172800 IN DNSKEY 257 3 8 AwEAAaz/tAm8yTn4Mfeh5eyI96WSVexTBAvkMgJzkKTOiW1vkIbzxeF3",
				}
			})

			It("should create resolver with custom trust anchors", func() {
				resolver, err := NewDNSSECResolver(ctx, sutConfig, mockUpstream)
				Expect(err).Should(Succeed())
				Expect(resolver).ShouldNot(BeNil())

				// Type assert to access validator field
				dnssecResolver, ok := resolver.(*DNSSECResolver)
				Expect(ok).Should(BeTrue())
				Expect(dnssecResolver.validator).ShouldNot(BeNil())
			})
		})

		When("invalid trust anchor is provided", func() {
			BeforeEach(func() {
				sutConfig.Validate = true
				sutConfig.TrustAnchors = []string{
					"invalid trust anchor string",
				}
			})

			It("should return error", func() {
				resolver, err := NewDNSSECResolver(ctx, sutConfig, mockUpstream)
				Expect(err).Should(HaveOccurred())
				Expect(resolver).Should(BeNil())
			})
		})
	})

	Describe("Resolve", func() {
		var (
			request  *model.Request
			response *dns.Msg
		)

		BeforeEach(func() {
			// Create a basic DNS request
			req := util.NewMsgWithQuestion("example.com.", A)
			request = &model.Request{
				Req:      req,
				Protocol: model.RequestProtocolUDP,
			}

			// Create a basic DNS response
			response, _ = util.NewMsgWithAnswer("example.com.", 300, A, "192.0.2.1")
		})

		When("DNSSEC validation is disabled", func() {
			BeforeEach(func() {
				sutConfig.Validate = false
			})

			JustBeforeEach(func() {
				resolver, err := NewDNSSECResolver(ctx, sutConfig, mockUpstream)
				Expect(err).Should(Succeed())

				sut, _ = resolver.(*DNSSECResolver)
				sut.Next(mockUpstream)
			})

			It("should pass through response without modification", func() {
				mockUpstream.On("Resolve", mock.Anything).Return(&model.Response{Res: response}, nil)

				resp, err := sut.Resolve(ctx, request)
				Expect(err).Should(Succeed())
				Expect(resp).ShouldNot(BeNil())
				Expect(resp.Res.Answer).Should(HaveLen(1))
			})

			It("should not set DO bit", func() {
				mockUpstream.On("Resolve", mock.Anything).Return(&model.Response{Res: response}, nil)

				_, err := sut.Resolve(ctx, request)
				Expect(err).Should(Succeed())

				// Verify DO bit was not set
				opt := request.Req.IsEdns0()
				if opt != nil {
					Expect(opt.Do()).Should(BeFalse())
				}
			})
		})

		When("DNSSEC validation is enabled", func() {
			BeforeEach(func() {
				sutConfig.Validate = true
			})

			JustBeforeEach(func() {
				resolver, err := NewDNSSECResolver(ctx, sutConfig, mockUpstream)
				Expect(err).Should(Succeed())

				sut, _ = resolver.(*DNSSECResolver)
				sut.Next(mockUpstream)
			})

			It("should set DO bit on requests", func() {
				mockUpstream.On("Resolve", mock.Anything).Return(&model.Response{Res: response}, nil)

				_, err := sut.Resolve(ctx, request)
				Expect(err).Should(Succeed())

				// Verify DO bit was set
				opt := request.Req.IsEdns0()
				Expect(opt).ShouldNot(BeNil())
				Expect(opt.Do()).Should(BeTrue())
			})

			It("should clear AD flag for insecure responses", func() {
				// Response without DNSSEC records
				response.AuthenticatedData = true // Upstream set it, but we should clear it

				mockUpstream.On("Resolve", mock.Anything).Return(&model.Response{Res: response}, nil)

				resp, err := sut.Resolve(ctx, request)
				Expect(err).Should(Succeed())
				Expect(resp.Res.AuthenticatedData).Should(BeFalse())
			})

			It("should return SERVFAIL when validation is bogus", func() {
				// Response with expired RRSIG
				response.Answer = append(response.Answer, &dns.RRSIG{
					Hdr: dns.RR_Header{
						Name:   "example.com.",
						Rrtype: dns.TypeRRSIG,
						Class:  dns.ClassINET,
						Ttl:    300,
					},
					TypeCovered: dns.TypeA,
					Algorithm:   8,
					Labels:      2,
					OrigTtl:     300,
					Expiration:  uint32(time.Now().Add(-48 * time.Hour).Unix()), // Expired
					Inception:   uint32(time.Now().Add(-72 * time.Hour).Unix()),
					KeyTag:      12345,
					SignerName:  "example.com.",
					Signature:   "invalid",
				})

				// Mock empty DNSKEY response (missing DNSKEY = Bogus per RFC 4035)
				dnskeyResp := new(dns.Msg)
				dnskeyResp.SetRcode(&dns.Msg{}, dns.RcodeSuccess)
				mockUpstream.On("Resolve", mock.Anything).Return(&model.Response{Res: response}, nil).Once()
				mockUpstream.On("Resolve", mock.Anything).Return(&model.Response{Res: dnskeyResp}, nil)

				resp, err := sut.Resolve(ctx, request)
				Expect(err).Should(Succeed())
				// Should return SERVFAIL per RFC 4035: RRSIG present + missing DNSKEY = Bogus = SERVFAIL
				Expect(resp.Res.Rcode).Should(Equal(dns.RcodeServerFailure))
				Expect(resp.Res.AuthenticatedData).Should(BeFalse())
			})

			DescribeTable("tells the client why validation failed with an extended DNS error",
				func(keys *dns.Msg, code uint16, text string) {
					mockUpstream.On("Resolve", mock.Anything).Return(&model.Response{Res: response}, nil).Once()
					if keys == nil {
						mockUpstream.On("Resolve", mock.Anything).Return(nil, errors.New("i/o timeout"))
					} else {
						mockUpstream.On("Resolve", mock.Anything).Return(&model.Response{Res: keys}, nil)
					}
					resp, err := sut.Resolve(ctx, request)
					Expect(err).Should(Succeed())
					Expect(resp.Res.Rcode).Should(Equal(dns.RcodeServerFailure))
					Expect(resp.Res.Answer).Should(BeEmpty())
					Expect(resp.RType).Should(Equal(model.ResponseTypeBOGUS))
					Expect(resp.Reason).Should(Equal("DNSSEC validation failed: " + text))
					ede := util.GetEdns0Option[*dns.EDNS0_EDE](resp.Res)
					Expect(ede).ShouldNot(BeNil())
					Expect(ede.InfoCode).Should(Equal(code))
					Expect(ede.ExtraText).Should(Equal(resp.Reason))
				},
				Entry("unanswered DNSKEY lookup", (*dns.Msg)(nil), dns.ExtendedErrorCodeNoReachableAuthority,
					"no reply to the DNSKEY query for ."),
				Entry("truncated DNSKEY reply", &dns.Msg{Truncated: true}, dns.ExtendedErrorCodeNetworkError,
					"truncated reply to the DNSKEY query for ."),
				Entry("DNSKEY server failure", &dns.Msg{Rcode: dns.RcodeServerFailure}, dns.ExtendedErrorCodeDNSKEYMissing,
					"SERVFAIL reply to the DNSKEY query for ."),
				Entry("DNSKEY reply without keys", &dns.Msg{}, dns.ExtendedErrorCodeDNSKEYMissing,
					"no records of the DNSKEY RRset in the reply to the DNSKEY query for ."),
			)

			// Public-upstream answers (RESOLVED) and cached upstream answers (CACHED) must be
			// validated (GHSA-x845-2f78-7v36). Trusted-local / synthesized answers -
			// conditional-upstream private zones, special-use names like localhost, and
			// DNS64-synthesized AAAA - are inherently unsigned and must be exempted, like the
			// rebinding resolver's response-type whitelist, instead of being turned into SERVFAIL.
			DescribeTable("skips validation for trusted-local / synthesized responses (whitelist carve-out)",
				func(rType model.ResponseType) {
					// unsigned answer; the local resolver may even assert AD, which we must not propagate
					response.AuthenticatedData = true
					mockUpstream.On("Resolve", mock.Anything).Return(
						&model.Response{Res: response, RType: rType}, nil)

					resp, err := sut.Resolve(ctx, request)
					Expect(err).Should(Succeed())
					// passes through untouched - not turned into SERVFAIL
					Expect(resp.Res.Rcode).Should(Equal(dns.RcodeSuccess))
					Expect(resp.Res.Answer).Should(HaveLen(1))
					Expect(resp.RType).Should(Equal(rType))
					// AD cleared: we did not authenticate it
					Expect(resp.Res.AuthenticatedData).Should(BeFalse())
					// the carve-out short-circuits before the validator runs, so no DS/DNSKEY
					// sub-queries are issued - only the initial next.Resolve hit the upstream
					Expect(mockUpstream.Calls).Should(HaveLen(1))
				},
				Entry("conditional upstream - private/split-horizon zone", model.ResponseTypeCONDITIONAL),
				Entry("special-use domain name - e.g. localhost", model.ResponseTypeSPECIAL),
				Entry("DNS64-synthesized AAAA", model.ResponseTypeSYNTHESIZED),
			)

			// Public-upstream answers must remain fully validated (GHSA-x845-2f78-7v36),
			// including cached upstream answers re-served as CACHED (re-validated on every
			// cache hit). The whitelist carve-out above is strictly response-type scoped
			// and must not weaken this.
			DescribeTable("rejects unsigned public-upstream responses with SERVFAIL",
				func(rType model.ResponseType) {
					mockUpstream.On("Resolve", mock.Anything).Return(
						&model.Response{Res: response, RType: rType}, nil)

					resp, err := sut.Resolve(ctx, request)
					Expect(err).Should(Succeed())
					// unsigned answer under the default root anchor -> bogus -> SERVFAIL
					Expect(resp.Res.Rcode).Should(Equal(dns.RcodeServerFailure))
				},
				Entry("resolved - public upstream", model.ResponseTypeRESOLVED),
				Entry("cached upstream answer - re-validated on hit", model.ResponseTypeCACHED),
			)

			It("passes answers to RRSIG questions without validation, but validates their denials", func() {
				request.Req = util.NewMsgWithQuestion("example.com.", dns.Type(dns.TypeRRSIG))
				rrsig := func(owner string) dns.RR {
					return &dns.RRSIG{
						Hdr:         dns.RR_Header{Name: owner, Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: 300},
						TypeCovered: dns.TypeA, Algorithm: dns.ECDSAP256SHA256, Labels: 2, OrigTtl: 300,
						KeyTag: 12345, SignerName: "example.com.", Signature: "invalid",
					}
				}
				alias, _ := util.NewMsgWithAnswer("example.com.", 300, CNAME, "target.example.com.")
				other, _ := util.NewMsgWithAnswer("example.com.", 300, A, "192.0.2.1")
				answers := [][]dns.RR{{rrsig("example.com.")}, {alias.Answer[0], rrsig("target.example.com.")}, other.Answer}
				for _, answer := range answers {
					res := &dns.Msg{Answer: answer}
					res.AuthenticatedData = true
					mockUpstream.On("Resolve", mock.Anything).Return(
						&model.Response{Res: res, RType: model.ResponseTypeRESOLVED}, nil).Once()
				}
				mockUpstream.On("Resolve", mock.Anything).Return(
					&model.Response{Res: new(dns.Msg), RType: model.ResponseTypeRESOLVED}, nil)
				for i, answer := range answers {
					resp, err := sut.Resolve(ctx, request)
					Expect(err).Should(Succeed())
					Expect(resp.Res.Rcode).Should(Equal(dns.RcodeSuccess))
					Expect(resp.Res.Answer).Should(Equal(answer))
					Expect(resp.Res.AuthenticatedData).Should(BeFalse())
					Expect(mockUpstream.Calls).Should(HaveLen(i + 1))
				}
				resp, err := sut.Resolve(ctx, request)
				Expect(err).Should(Succeed())
				Expect(resp.Res.Rcode).Should(Equal(dns.RcodeServerFailure))
			})

			When("the client sets the CD bit", func() {
				BeforeEach(func() {
					request.Req.CheckingDisabled = true
				})

				DescribeTable("returns the upstream answer without validating it",
					func(rType model.ResponseType) {
						// unsigned answer under the default root anchor: bogus without CD
						response.AuthenticatedData = true
						mockUpstream.On("Resolve", mock.Anything).Return(
							&model.Response{Res: response, RType: rType}, nil)

						resp, err := sut.Resolve(ctx, request)
						Expect(err).Should(Succeed())
						Expect(resp.Res.Rcode).Should(Equal(dns.RcodeSuccess))
						Expect(resp.Res.Answer).Should(HaveLen(1))
						Expect(resp.RType).Should(Equal(rType))
						Expect(resp.Res.AuthenticatedData).Should(BeFalse())
						// no DS/DNSKEY sub-queries, and the CD bit went upstream with the request
						Expect(mockUpstream.Calls).Should(HaveLen(1))
						forwarded, ok := mockUpstream.Calls[0].Arguments.Get(0).(*model.Request)
						Expect(ok).Should(BeTrue())
						Expect(forwarded.Req.CheckingDisabled).Should(BeTrue())
					},
					Entry("resolved - public upstream", model.ResponseTypeRESOLVED),
					Entry("cached upstream answer", model.ResponseTypeCACHED),
				)

				It("returns data with a broken signature instead of SERVFAIL", func() {
					response.Answer = append(response.Answer, &dns.RRSIG{
						Hdr: dns.RR_Header{
							Name:   "example.com.",
							Rrtype: dns.TypeRRSIG,
							Class:  dns.ClassINET,
							Ttl:    300,
						},
						TypeCovered: dns.TypeA,
						Algorithm:   8,
						Labels:      2,
						OrigTtl:     300,
						Expiration:  uint32(time.Now().Add(-48 * time.Hour).Unix()),
						Inception:   uint32(time.Now().Add(-72 * time.Hour).Unix()),
						KeyTag:      12345,
						SignerName:  "example.com.",
						Signature:   "invalid",
					})
					mockUpstream.On("Resolve", mock.Anything).Return(
						&model.Response{Res: response, RType: model.ResponseTypeRESOLVED}, nil)

					resp, err := sut.Resolve(ctx, request)
					Expect(err).Should(Succeed())
					Expect(resp.Res.Rcode).Should(Equal(dns.RcodeSuccess))
					Expect(resp.Res.Answer).Should(HaveLen(2))
					Expect(resp.Res.AuthenticatedData).Should(BeFalse())
				})
			})
		})

		When("upstream resolver returns error", func() {
			JustBeforeEach(func() {
				resolver, err := NewDNSSECResolver(ctx, sutConfig, mockUpstream)
				Expect(err).Should(Succeed())

				sut, _ = resolver.(*DNSSECResolver)
				sut.Next(mockUpstream)
			})

			It("should return the error", func() {
				mockUpstream.On("Resolve", mock.Anything).Return(nil, dns.ErrTime)

				resp, err := sut.Resolve(ctx, request)
				Expect(err).Should(HaveOccurred())
				Expect(resp).Should(BeNil())
			})
		})
	})

	Describe("LogConfig", func() {
		BeforeEach(func() {
			sutConfig.Validate = false
		})

		JustBeforeEach(func() {
			resolver, err := NewDNSSECResolver(ctx, sutConfig, mockUpstream)
			Expect(err).Should(Succeed())

			sut, _ = resolver.(*DNSSECResolver)
		})

		It("should log configuration", func() {
			logger, hook := log.NewMockLogger()

			sut.LogConfig(logger)

			Expect(hook.Records()).ShouldNot(BeEmpty())
		})
	})

	Describe("EDNS0 buffer size handling", func() {
		var response *dns.Msg

		BeforeEach(func() {
			sutConfig.Validate = true
			response, _ = util.NewMsgWithAnswer("example.com.", 300, A, "192.0.2.1")
		})

		JustBeforeEach(func() {
			resolver, err := NewDNSSECResolver(ctx, sutConfig, mockUpstream)
			Expect(err).Should(Succeed())

			sut, _ = resolver.(*DNSSECResolver)
			sut.Next(mockUpstream)
		})

		It("should increase EDNS0 buffer size if too small", func() {
			req := util.NewMsgWithQuestion("example.com.", A)
			// Add EDNS0 with small buffer
			req.SetEdns0(512, false)

			request := &model.Request{
				Req:      req,
				Protocol: model.RequestProtocolUDP,
			}

			mockUpstream.On("Resolve", mock.Anything).Return(&model.Response{Res: response}, nil)

			_, err := sut.Resolve(ctx, request)
			Expect(err).Should(Succeed())

			// Verify buffer size was increased
			opt := request.Req.IsEdns0()
			Expect(opt).ShouldNot(BeNil())
			Expect(opt.UDPSize()).Should(BeNumerically(">=", 4096))
			Expect(opt.Do()).Should(BeTrue())
		})

		It("should preserve existing EDNS0 buffer size if adequate", func() {
			req := util.NewMsgWithQuestion("example.com.", A)
			// Add EDNS0 with large buffer
			req.SetEdns0(8192, false)

			request := &model.Request{
				Req:      req,
				Protocol: model.RequestProtocolUDP,
			}

			mockUpstream.On("Resolve", mock.Anything).Return(&model.Response{Res: response}, nil)

			_, err := sut.Resolve(ctx, request)
			Expect(err).Should(Succeed())

			// Verify buffer size was preserved
			opt := request.Req.IsEdns0()
			Expect(opt).ShouldNot(BeNil())
			Expect(opt.UDPSize()).Should(Equal(uint16(8192)))
			Expect(opt.Do()).Should(BeTrue())
		})
	})

	Describe("createServFailResponseDNSSEC", func() {
		It("should create SERVFAIL response with EDE", func() {
			req := util.NewMsgWithQuestion("example.com.", A)
			reason := "DNSSEC validation failed: bogus signatures"
			modelReq := &model.Request{Req: req}

			response := createServFailResponseDNSSEC(modelReq, dns.ExtendedErrorCodeDNSBogus, reason)

			Expect(response).ShouldNot(BeNil())
			Expect(response.Res.Rcode).Should(Equal(dns.RcodeServerFailure))
			Expect(response.Reason).Should(Equal(reason))
			// a validation failure is an error, not a block: it must not be counted
			// as one in the statistics, nor reported to the client as EDE "Blocked"
			Expect(response.RType).Should(Equal(model.ResponseTypeBOGUS))
			Expect(response.RType.ToExtendedErrorCode()).Should(Equal(dns.ExtendedErrorCodeDNSBogus))

			// Check for EDNS0 with EDE
			opt := response.Res.IsEdns0()
			Expect(opt).ShouldNot(BeNil())

			// Verify EDE option exists
			edeFound := false
			for _, option := range opt.Option {
				if ede, ok := option.(*dns.EDNS0_EDE); ok {
					Expect(ede.InfoCode).Should(Equal(dns.ExtendedErrorCodeDNSBogus))
					Expect(ede.ExtraText).Should(Equal(reason))
					edeFound = true
				}
			}
			Expect(edeFound).Should(BeTrue())
		})

		It("should set proper EDNS0 UDP size", func() {
			req := util.NewMsgWithQuestion("example.com.", A)
			modelReq := &model.Request{Req: req}

			response := createServFailResponseDNSSEC(modelReq, dns.ExtendedErrorCodeDNSBogus, "test reason")

			opt := response.Res.IsEdns0()
			Expect(opt).ShouldNot(BeNil())
			Expect(opt.UDPSize()).Should(Equal(uint16(4096)))
		})
	})

	Describe("Resolve with validation edge cases", func() {
		BeforeEach(func() {
			sutConfig.Validate = true
		})

		JustBeforeEach(func() {
			resolver, err := NewDNSSECResolver(ctx, sutConfig, mockUpstream)
			Expect(err).Should(Succeed())

			sut, _ = resolver.(*DNSSECResolver)
			sut.Next(mockUpstream)
		})

		It("should handle response without question section", func() {
			req := util.NewMsgWithQuestion("example.com.", A)
			request := &model.Request{
				Req:      req,
				Protocol: model.RequestProtocolUDP,
			}

			// Response without question section
			emptyResponse := &dns.Msg{}

			mockUpstream.On("Resolve", mock.Anything).Return(&model.Response{Res: emptyResponse}, nil)

			resp, err := sut.Resolve(ctx, request)
			Expect(err).Should(Succeed())
			Expect(resp).ShouldNot(BeNil())
		})

		When("a signed zone is anchored", func() {
			var (
				key           *dns.DNSKEY
				signed        func(dns.RR) []dns.RR
				a             dns.RR
				replies, keys []*dns.Msg
				asked         map[uint16]int
				request       *model.Request
			)
			newRR := func(text string) dns.RR {
				rr, err := dns.NewRR(text)
				Expect(err).Should(Succeed())

				return rr
			}
			told := func(resp *model.Response) *dns.EDNS0_EDE {
				Expect(resp.Res.Rcode).Should(Equal(dns.RcodeServerFailure))
				Expect(resp.RType).Should(Equal(model.ResponseTypeBOGUS))
				ede := util.GetEdns0Option[*dns.EDNS0_EDE](resp.Res)
				Expect(ede).ShouldNot(BeNil())

				return ede
			}
			resolve := func() *model.Response {
				resp, err := sut.Resolve(ctx, request)
				Expect(err).Should(Succeed())

				return resp
			}
			kept := func() (*validationFailure, time.Duration) {
				return sut.failureCache.Get(failureCacheKey(request, "", nil))
			}
			leave := func(left time.Duration) {
				failure, _ := kept()
				Expect(failure).ShouldNot(BeNil())
				sut.failureCache.Put(failureCacheKey(request, "", nil), failure, left)
			}
			withCache := func(cfg config.Caching) *CachingResolver {
				cache, err := NewCachingResolver(ctx, cfg, sutConfig)
				Expect(err).Should(Succeed())
				cache.Next(mockUpstream)
				sut.Next(cache)

				return cache
			}
			BeforeEach(func() {
				key = &dns.DNSKEY{
					Hdr:   dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 300},
					Flags: 257, Protocol: 3, Algorithm: dns.ECDSAP256SHA256,
				}
				priv, err := key.Generate(256)
				for err == nil && key.KeyTag() == 0 {
					priv, err = key.Generate(256)
				}
				Expect(err).Should(Succeed())
				signed = func(rr dns.RR) []dns.RR {
					sig := &dns.RRSIG{
						Hdr:       dns.RR_Header{Ttl: rr.Header().Ttl},
						Algorithm: key.Algorithm, KeyTag: key.KeyTag(), SignerName: key.Hdr.Name,
						Inception:  uint32(time.Now().Add(-time.Hour).Unix()),
						Expiration: uint32(time.Now().Add(time.Hour).Unix()),
					}
					Expect(sig.Sign(priv.(crypto.Signer), []dns.RR{rr})).Should(Succeed())

					return []dns.RR{rr, sig}
				}
				sutConfig.TrustAnchors = []string{key.String()}
				a = newRR("example.com. 300 IN A 192.0.2.1")
				replies = []*dns.Msg{{Answer: signed(a)}}
				keys = []*dns.Msg{{Answer: signed(key)}}
				asked = map[uint16]int{}
				request = &model.Request{Req: util.NewMsgWithQuestion("example.com.", A)}
				noDS := &dns.Msg{Ns: signed(newRR("unsigned.example.com. 300 IN NSEC www.example.com. NS RRSIG NSEC"))}
				mockUpstream.On("Resolve", mock.Anything)
				mockUpstream.ResolveFn = func(_ context.Context, req *model.Request) (*model.Response, error) {
					qtype := req.Req.Question[0].Qtype
					asked[qtype]++
					from := &replies
					switch qtype {
					case dns.TypeDNSKEY:
						from = &keys
					case dns.TypeDS:
						from = &[]*dns.Msg{noDS}
					}
					reply := (*from)[0]
					if len(*from) > 1 {
						*from = (*from)[1:]
					}
					if reply == nil {
						return nil, errors.New("i/o timeout")
					}

					return &model.Response{Res: reply.Copy(), RType: model.ResponseTypeRESOLVED}, nil
				}
			})

			It("removes orphan signatures, unsigned authority NS and additional data only with AD", func() {
				replies[0].Answer = append(replies[0].Answer, signed(newRR("example.com. 300 IN AAAA 2001:db8::1"))[1])
				replies[0].Ns = append([]dns.RR{newRR("example.com. 300 IN NS ns.example.com.")},
					signed(newRR("example.com. 300 IN SOA ns.example.com. hostmaster.example.com. 1 3600 600 86400 300"))[1])
				replies[0].Extra = []dns.RR{newRR("ns.example.com. 300 IN A 192.0.2.25")}
				replies[0].SetEdns0(ednsUDPSize, true)
				resp := resolve()
				Expect(resp.Res.AuthenticatedData).Should(BeTrue())
				Expect(resp.Res.Answer).Should(HaveLen(2))
				Expect(resp.Res.Answer[1].(*dns.RRSIG).TypeCovered).Should(Equal(dns.TypeA))
				Expect(resp.Res.Ns).Should(BeEmpty())
				Expect(resp.Res.Extra).Should(HaveLen(1))
				Expect(resp.Res.IsEdns0()).ShouldNot(BeNil())
				request.Req.CheckingDisabled = true
				resp = resolve()
				Expect(resp.Res.AuthenticatedData).Should(BeFalse())
				Expect(resp.Res.Answer).Should(HaveLen(3))
				Expect(resp.Res.Ns).Should(HaveLen(2))
				Expect(resp.Res.Extra).Should(HaveLen(2))
			})

			It("bounds answer and authority TTLs by their proof without changing CD data", func() {
				for _, rr := range replies[0].Answer {
					rr.Header().Ttl = 86400
				}
				replies[0].Ns = signed(newRR("example.com. 600 IN NS ns.example.com."))
				for _, rr := range replies[0].Ns {
					rr.Header().Ttl = 120
				}
				resp := resolve()
				Expect(resp.Res.AuthenticatedData).Should(BeTrue())
				for _, rr := range resp.Res.Answer {
					Expect(rr.Header().Ttl).Should(Equal(uint32(300)))
				}
				for _, rr := range resp.Res.Ns {
					Expect(rr.Header().Ttl).Should(Equal(uint32(120)))
				}
				request.Req.CheckingDisabled = true
				for _, rr := range resolve().Res.Answer {
					Expect(rr.Header().Ttl).Should(Equal(uint32(86400)))
				}
			})

			It("bounds both delivery and cache reuse by the signed TTL despite a higher cache minimum", func() {
				cache := withCache(config.Caching{MinCachingTime: config.Duration(time.Hour)})
				for range 2 {
					resp := resolve()
					Expect(resp.Res.AuthenticatedData).Should(BeTrue())
					Expect(resp.Res.Answer).Should(HaveLen(2))
					for _, rr := range resp.Res.Answer {
						Expect(rr.Header().Ttl).Should(BeNumerically("~", 300, 1))
						Expect(rr.Header().Ttl).Should(BeNumerically("<=", 300))
					}
				}
				Expect(asked[dns.TypeA]).Should(Equal(1))
				held, err := cache.Resolve(ctx, request)
				Expect(err).Should(Succeed())
				Expect(held.RType).Should(Equal(model.ResponseTypeCACHED))
				for _, rr := range held.Res.Answer {
					Expect(rr.Header().Ttl).Should(BeNumerically(">", 0))
					Expect(rr.Header().Ttl).Should(BeNumerically("<=", 300))
				}
			})

			DescribeTable("recovers from a transient failure and then uses the normal response cache",
				func(badKeys bool) {
					withCache(config.Caching{})
					if badKeys {
						keys = []*dns.Msg{{Answer: []dns.RR{key}}, {Answer: signed(key)}}
					} else {
						replies = []*dns.Msg{{Answer: []dns.RR{a}}, {Answer: signed(a)}}
					}
					for range 2 {
						resp, err := sut.Resolve(ctx, &model.Request{Req: util.NewMsgWithQuestion("example.com.", A)})
						Expect(err).Should(Succeed())
						Expect(resp.Res.AuthenticatedData).Should(BeTrue())
					}
					Expect(asked[dns.TypeA]).Should(Equal(2))
					Expect(sut.failureCache.TotalCount()).Should(BeZero())
				}, Entry("answer missing its signature", false), Entry("DNSKEY missing its signature", true))

			DescribeTable("keeps the last validation error after two failed answers",
				func(noSecond bool) {
					second := &dns.Msg{Answer: []dns.RR{newRR("example.com. 300 IN A 203.0.113.66"), signed(a)[1]}}
					code := dns.ExtendedErrorCodeDNSBogus
					if noSecond {
						second = nil
						code = dns.ExtendedErrorCodeRRSIGsMissing
					}
					replies = []*dns.Msg{{Answer: []dns.RR{a}}, second, {Answer: signed(a)}}
					for range 2 {
						Expect(told(resolve()).InfoCode).Should(Equal(code))
					}
					Expect(asked[dns.TypeA]).Should(Equal(2))
					failure, left := kept()
					Expect(failure.refused).Should(Equal(time.Second))
					Expect(left).Should(BeNumerically("~", time.Second+failureMemory, time.Second))
				}, Entry("second answer is forged", false), Entry("second answer never arrives", true))

			DescribeTable("refuses persistent failures exponentially for at most one minute",
				func(cached bool) {
					if cached {
						withCache(config.Caching{})
					}
					replies = []*dns.Msg{{Answer: []dns.RR{a}}}
					for row, refused := range []time.Duration{
						time.Second, 2 * time.Second, 4 * time.Second,
						8 * time.Second, 16 * time.Second, 32 * time.Second, dnssec.BogusTTL, dnssec.BogusTTL,
					} {
						for range 2 {
							Expect(told(resolve()).InfoCode).Should(Equal(dns.ExtendedErrorCodeRRSIGsMissing))
						}
						if cached {
							Expect(asked[dns.TypeA]).Should(Equal(row + 2))
						} else {
							Expect(asked[dns.TypeA]).Should(Equal(2 * (row + 1)))
						}
						failure, left := kept()
						Expect(failure.refused).Should(Equal(refused))
						Expect(left).Should(BeNumerically("~", refused+failureMemory, time.Second))
						leave(failureMemory)
					}
				}, Entry("with a response cache", true), Entry("without a response cache", false))

			DescribeTable("ends a row of failures after recovery or expiry",
				func(second, expires bool) {
					replies = []*dns.Msg{{Answer: []dns.RR{a}}}
					for range 2 {
						told(resolve())
						leave(failureMemory)
					}
					failure, _ := kept()
					Expect(failure.refused).Should(Equal(2 * time.Second))
					if expires {
						leave(time.Millisecond)
						Eventually(func() time.Duration {
							_, left := kept()

							return left
						}).Should(BeNumerically("<=", 0))
					} else {
						replies = []*dns.Msg{{Answer: signed(a)}}
						if second {
							replies = append([]*dns.Msg{{Answer: []dns.RR{a}}}, replies...)
						}
						Expect(resolve().Res.AuthenticatedData).Should(BeTrue())
						Expect(sut.failureCache.TotalCount()).Should(BeZero())
					}
					replies = []*dns.Msg{{Answer: []dns.RR{a}}}
					told(resolve())
					failure, _ = kept()
					Expect(failure.refused).Should(Equal(time.Second))
				}, Entry("first answer recovers", false, false), Entry("second answer recovers", true, false),
				Entry("failure memory expires", false, true))

			DescribeTable("does not repeat unanswered lookups but retries incomplete DNSKEY replies within limits",
				func(firstReply, repliesOnly bool) {
					keys = []*dns.Msg{nil, {Answer: signed(key)}}
					want := map[uint16]int{dns.TypeA: 1, dns.TypeDNSKEY: 1}
					code := dns.ExtendedErrorCodeNoReachableAuthority
					if firstReply {
						keys = append([]*dns.Msg{{Rcode: dns.RcodeServerFailure}}, keys...)
						want[dns.TypeDNSKEY] = 2
					}
					if repliesOnly {
						keys = []*dns.Msg{{Rcode: dns.RcodeServerFailure}}
						want = map[uint16]int{dns.TypeA: 2, dns.TypeDNSKEY: 4}
						code = dns.ExtendedErrorCodeDNSKEYMissing
					}
					Expect(told(resolve()).InfoCode).Should(Equal(code))
					Expect(asked).Should(Equal(want))
					Expect(told(resolve()).InfoCode).Should(Equal(code))
					Expect(asked).Should(Equal(want))
					failure, _ := kept()
					Expect(failure.refused).Should(Equal(time.Second))
					if !repliesOnly {
						leave(failureMemory)
						Expect(resolve().Res.AuthenticatedData).Should(BeTrue())
						Expect(sut.failureCache.TotalCount()).Should(BeZero())
					}
				}, Entry("first DNSKEY lookup unanswered", false, false),
				Entry("second DNSKEY lookup unanswered", true, false), Entry("both DNSKEY replies incomplete", true, true))

			It("does not repeat a DS lookup that received no reply", func() {
				request.Req = util.NewMsgWithQuestion("www.unsigned.example.com.", A)
				replies = []*dns.Msg{{Answer: []dns.RR{newRR("www.unsigned.example.com. 300 IN A 192.0.2.1")}}}
				upstream := mockUpstream.ResolveFn
				mockUpstream.ResolveFn = func(ctx context.Context, req *model.Request) (*model.Response, error) {
					if req.Req.Question[0].Qtype == dns.TypeDS {
						asked[dns.TypeDS]++

						return nil, errors.New("i/o timeout")
					}

					return upstream(ctx, req)
				}
				Expect(told(resolve()).InfoCode).Should(Equal(dns.ExtendedErrorCodeNoReachableAuthority))
				Expect(asked).Should(Equal(map[uint16]int{dns.TypeA: 1, dns.TypeDNSKEY: 1, dns.TypeDS: 1}))
				Expect(sut.failureCache.TotalCount()).Should(Equal(1))
			})

			DescribeTable("bounds retries of incomplete DS replies and keeps their specific extended error",
				func(reply *dns.Msg, code uint16) {
					request.Req = util.NewMsgWithQuestion("www.unsigned.example.com.", A)
					answer := newRR("www.unsigned.example.com. 300 IN A 192.0.2.1")
					replies = []*dns.Msg{{Answer: []dns.RR{answer}}}
					upstream := mockUpstream.ResolveFn
					mockUpstream.ResolveFn = func(ctx context.Context, req *model.Request) (*model.Response, error) {
						if req.Req.Question[0].Qtype == dns.TypeDS {
							asked[dns.TypeDS]++

							return &model.Response{Res: reply.Copy(), RType: model.ResponseTypeRESOLVED}, nil
						}

						return upstream(ctx, req)
					}
					Expect(told(resolve()).InfoCode).Should(Equal(code))
					Expect(asked).Should(Equal(map[uint16]int{dns.TypeA: 2, dns.TypeDNSKEY: 1, dns.TypeDS: 4}))
					Expect(told(resolve()).InfoCode).Should(Equal(code))
					Expect(asked[dns.TypeDS]).Should(Equal(4))
				}, Entry("SERVFAIL", &dns.Msg{Rcode: dns.RcodeServerFailure}, dns.ExtendedErrorCodeNetworkError),
				Entry("truncated", &dns.Msg{Truncated: true}, dns.ExtendedErrorCodeNetworkError),
				Entry("missing denial", &dns.Msg{}, dns.ExtendedErrorCodeNSECMissing))

			It("does not retry when another signer's lookup had no reply beside a bad signature", func() {
				sub := &dns.DNSKEY{
					Hdr:   dns.RR_Header{Name: "sub.example.com.", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 300},
					Flags: 257, Protocol: 3, Algorithm: dns.ECDSAP256SHA256,
				}
				private, err := sub.Generate(256)
				for err == nil && sub.KeyTag() == 0 {
					private, err = sub.Generate(256)
				}
				Expect(err).Should(Succeed())
				host := newRR("www.sub.example.com. 300 IN A 192.0.2.1")
				bySub := &dns.RRSIG{
					Hdr:       dns.RR_Header{Ttl: 300},
					Algorithm: sub.Algorithm, KeyTag: sub.KeyTag(), SignerName: sub.Hdr.Name,
					Inception:  uint32(time.Now().Add(-time.Hour).Unix()),
					Expiration: uint32(time.Now().Add(time.Hour).Unix()),
				}
				Expect(bySub.Sign(private.(crypto.Signer), []dns.RR{host})).Should(Succeed())
				ofOtherData := signed(newRR("www.sub.example.com. 300 IN A 203.0.113.66"))[1]
				replies = []*dns.Msg{{Answer: []dns.RR{host, ofOtherData, bySub}}, {Answer: signed(host)}}
				request.Req = util.NewMsgWithQuestion("www.sub.example.com.", A)
				upstream := mockUpstream.ResolveFn
				mockUpstream.ResolveFn = func(ctx context.Context, req *model.Request) (*model.Response, error) {
					if req.Req.Question[0].Qtype == dns.TypeDS {
						asked[dns.TypeDS]++

						return nil, errors.New("i/o timeout")
					}

					return upstream(ctx, req)
				}
				Expect(told(resolve()).InfoCode).Should(Equal(dns.ExtendedErrorCodeDNSBogus))
				Expect(asked).Should(Equal(map[uint16]int{dns.TypeA: 1, dns.TypeDS: 1, dns.TypeDNSKEY: 1}))
				failure, _ := kept()
				Expect(failure.refused).Should(Equal(time.Second))
			})

			It("counts overlapping failures as one refusal period", func() {
				replies = []*dns.Msg{{Answer: []dns.RR{a}}}
				upstream, others, other := mockUpstream.ResolveFn, 0, false
				mockUpstream.ResolveFn = func(ctx context.Context, req *model.Request) (*model.Response, error) {
					if req.AskedAgain && req.Req.Question[0].Qtype == dns.TypeA && !other {
						other = true
						others++
						resp, err := sut.Resolve(ctx, &model.Request{Req: util.NewMsgWithQuestion("example.com.", A)})
						Expect(err).Should(Succeed())
						told(resp)
						other = false
					}

					return upstream(ctx, req)
				}
				for _, refused := range []time.Duration{time.Second, 2 * time.Second} {
					told(resolve())
					failure, left := kept()
					Expect(failure.refused).Should(Equal(refused))
					Expect(left).Should(BeNumerically("~", refused+failureMemory, time.Second))
					leave(failureMemory)
				}
				Expect(others).Should(Equal(2))
				Expect(asked[dns.TypeA]).Should(Equal(8))
			})

			It("uses another client's cached failure before sending a second query", func() {
				forged := []dns.RR{newRR("example.com. 300 IN A 203.0.113.66"), signed(a)[1]}
				replies = []*dns.Msg{{Answer: forged}, {Answer: forged}, {Answer: []dns.RR{a}}, {Answer: signed(a)}}
				var theirs *validationFailure
				var left time.Duration
				upstream, other := mockUpstream.ResolveFn, false
				mockUpstream.ResolveFn = func(ctx context.Context, req *model.Request) (*model.Response, error) {
					if req.Req.Question[0].Qtype == dns.TypeA && !other {
						other = true
						resp, err := sut.Resolve(ctx, &model.Request{Req: util.NewMsgWithQuestion("example.com.", A)})
						Expect(err).Should(Succeed())
						Expect(told(resp).InfoCode).Should(Equal(dns.ExtendedErrorCodeDNSBogus))
						theirs, left = kept()
					}

					return upstream(ctx, req)
				}
				Expect(told(resolve()).InfoCode).Should(Equal(dns.ExtendedErrorCodeDNSBogus))
				Expect(asked[dns.TypeA]).Should(Equal(3))
				failure, now := kept()
				Expect(failure).Should(Equal(theirs))
				Expect(failure.refused).Should(Equal(time.Second))
				Expect(now).Should(BeNumerically("<=", left))
			})

			DescribeTable("clears only the failure observed before an accepted answer was requested",
				func(name string, answer func() []dns.RR) {
					_, logger := sut.log(ctx)
					validates := func() *askedQuery {
						q := new(askedQuery)
						resp, err := sut.answer(ctx, logger, request, q)
						Expect(err).Should(Succeed())
						Expect(resp.Res.Rcode).Should(Equal(dns.RcodeSuccess))

						return q
					}
					failure := &validationFailure{refused: time.Second}
					request.Req = util.NewMsgWithQuestion(name, A)
					replies = []*dns.Msg{{Answer: answer()}}
					other := &model.Request{Req: request.Req.Copy()}
					other.Req.RecursionDesired = false
					sut.failureCache.Put(failureCacheKey(other, "", nil), failure, failureMemory)
					Expect(validates().last).Should(BeNil())
					Expect(sut.failureCache.TotalCount()).Should(Equal(1))
					sut.failureCache.Put(failureCacheKey(request, "", nil), failure, failureMemory)
					Expect(validates().last).Should(Equal(failure))
					Expect(sut.failureCache.TotalCount()).Should(Equal(1))
					_, left := sut.failureCache.Get(failureCacheKey(other, "", nil))
					Expect(left).Should(BeNumerically(">", 0))
					upstream := mockUpstream.ResolveFn
					mockUpstream.ResolveFn = func(ctx context.Context, req *model.Request) (*model.Response, error) {
						if req.Req.Question[0].Qtype == dns.TypeA {
							sut.failureCache.Put(failureCacheKey(request, "", nil), failure, failureMemory)
						}

						return upstream(ctx, req)
					}
					Expect(validates().last).Should(BeNil())
					Expect(sut.failureCache.TotalCount()).Should(Equal(2))
				}, Entry("Secure", "example.com.", func() []dns.RR { return signed(a) }),
				Entry("Insecure", "www.unsigned.example.com.", func() []dns.RR {
					return []dns.RR{newRR("www.unsigned.example.com. 300 IN A 192.0.2.1")}
				}), Entry("Indeterminate", "example.org.", func() []dns.RR {
					return []dns.RR{newRR("example.org. 300 IN A 192.0.2.1")}
				}))

			It("only computes failure keys when the cache needs them", func() {
				_, logger := sut.log(ctx)
				valid := new(askedQuery)
				resp, err := sut.answer(ctx, logger, request, valid)
				Expect(err).Should(Succeed())
				Expect(resp.Res.AuthenticatedData).Should(BeTrue())
				Expect(valid.key).Should(BeEmpty())
				replies = []*dns.Msg{{Answer: []dns.RR{a}}}
				failing := new(askedQuery)
				resp, err = sut.answer(ctx, logger, request, failing)
				Expect(err).Should(Succeed())
				told(resp)
				Expect(failing.key).Should(HaveLen(sha256.Size))
				_, left := sut.failureCache.Get(failing.key)
				Expect(left).Should(BeNumerically(">", 0))
				next := new(askedQuery)
				resp, err = sut.answer(ctx, logger, request, next)
				Expect(err).Should(Succeed())
				told(resp)
				Expect(next.key).Should(Equal(failing.key))
			})

			DescribeTable("isolates a failed query from other query forms",
				func(change func(*dns.Msg), same bool) {
					replies = []*dns.Msg{{Answer: []dns.RR{a}}, {Answer: []dns.RR{a}}, {Answer: signed(a)}}
					first := util.NewMsgWithQuestion("example.com.", A)
					change(first)
					resp, err := sut.Resolve(ctx, &model.Request{Req: first})
					Expect(err).Should(Succeed())
					Expect(told(resp).InfoCode).Should(Equal(dns.ExtendedErrorCodeRRSIGsMissing))
					resp = resolve()
					if same {
						told(resp)
						Expect(asked[dns.TypeA]).Should(Equal(2))
					} else {
						Expect(resp.Res.AuthenticatedData).Should(BeTrue())
						Expect(asked[dns.TypeA]).Should(Equal(3))
					}
				}, Entry("same query", func(*dns.Msg) {}, true),
				Entry("different name case", func(q *dns.Msg) { q.Question[0].Name = "Example.COM." }, true),
				Entry("RD clear", func(q *dns.Msg) { q.RecursionDesired = false }, false),
				Entry("different class", func(q *dns.Msg) { q.Question[0].Qclass = dns.ClassCHAOS }, false),
				Entry("different EDNS version", func(q *dns.Msg) { q.SetEdns0(ednsUDPSize, true); q.IsEdns0().SetVersion(1) }, false),
				Entry("EDNS option", func(q *dns.Msg) {
					q.SetEdns0(ednsUDPSize, true)
					q.IsEdns0().Option = []dns.EDNS0{&dns.EDNS0_NSID{Code: dns.EDNS0NSID}}
				}, false))

			It("preserves view and client ECS isolation in fixed size failure keys", func() {
				query := func(options ...dns.EDNS0) *model.Request {
					msg := util.NewMsgWithQuestion("example.com.", A)
					msg.SetEdns0(ednsUDPSize, true)
					msg.IsEdns0().Option = options

					return &model.Request{Req: msg}
				}
				subnet := func(address string) *dns.EDNS0_SUBNET {
					return &dns.EDNS0_SUBNET{Code: dns.EDNS0SUBNET, Family: 1, SourceNetmask: 24, Address: net.ParseIP(address).To4()}
				}
				plain := failureCacheKey(query(), "", nil)
				Expect(plain).Should(HaveLen(sha256.Size))
				Expect(failureCacheKey(query(&dns.EDNS0_PADDING{Padding: make([]byte, 4000)}), "", nil)).
					Should(And(HaveLen(sha256.Size), Not(Equal(plain))))
				Expect(failureCacheKey(query(subnet("192.0.2.0")), "", nil)).Should(Equal(plain))
				sent := subnet("198.51.100.0")
				with := failureCacheKey(query(sent), "", sent)
				Expect(with).ShouldNot(Equal(plain))
				Expect(failureCacheKey(query(subnet("192.0.2.0")), "", sent)).Should(Equal(with))
				Expect(failureCacheKey(query(), "", sent)).Should(Equal(with))
				Expect(failureCacheKey(query(), "group", nil)).ShouldNot(Equal(plain))
			})

			It("never refuses cached validation failures to a client with CD", func() {
				replies = []*dns.Msg{{Answer: []dns.RR{a}}}
				told(resolve())
				request.Req.CheckingDisabled = true
				resp := resolve()
				Expect(resp.Res.Rcode).Should(Equal(dns.RcodeSuccess))
				Expect(resp.Res.Answer).Should(Equal([]dns.RR{a}))
				Expect(resp.Res.AuthenticatedData).Should(BeFalse())
				Expect(asked[dns.TypeA]).Should(Equal(3))
			})

			It("asks again when a successful replacement has zero TTL and cannot replace the old cache entry", func() {
				withCache(config.Caching{})
				replies = []*dns.Msg{{Answer: []dns.RR{a}}, {Answer: signed(newRR("example.com. 0 IN A 192.0.2.1"))}}
				for range 2 {
					resp, err := sut.Resolve(ctx, &model.Request{Req: util.NewMsgWithQuestion("example.com.", A)})
					Expect(err).Should(Succeed())
					Expect(resp.Res.AuthenticatedData).Should(BeTrue())
				}
				Expect(asked[dns.TypeA]).Should(Equal(3))
			})

			// DNS64 must synthesize only from validated inputs, including cache hits.
			When("DNS64 is above and the cache is below", func() {
				var dns64 ChainedResolver

				synthesized := func() *model.Response {
					resp, err := dns64.Resolve(ctx, &model.Request{Req: util.NewMsgWithQuestion("example.com.", AAAA)})
					Expect(err).Should(Succeed())

					return resp
				}

				JustBeforeEach(func() {
					// The cache needs a negative lifetime to retain the AAAA denial.
					negative := config.Duration(30 * time.Minute)
					withCache(config.Caching{CacheTimeNegative: negative})
					dns64 = NewDNS64Resolver(config.DNS64{Enable: true})
					dns64.Next(sut)

					replies = []*dns.Msg{{Answer: signed(a)}}
					noData := &dns.Msg{Ns: slices.Concat(
						signed(newRR("example.com. 300 IN SOA ns.example.com. hostmaster.example.com. 1 3600 600 86400 300")),
						signed(newRR("example.com. 300 IN NSEC unsigned.example.com. A NS SOA RRSIG NSEC DNSKEY")))}
					resolve := mockUpstream.ResolveFn
					mockUpstream.ResolveFn = func(ctx context.Context, req *model.Request) (*model.Response, error) {
						if req.Req.Question[0].Qtype != dns.TypeAAAA {
							return resolve(ctx, req)
						}

						asked[dns.TypeAAAA]++

						return &model.Response{Res: noData.Copy(), RType: model.ResponseTypeRESOLVED}, nil
					}
				})

				It("lets DNS64 synthesize from a denial and an A RRset that are authentic, also from the cache", func() {
					for range 2 {
						resp := synthesized()
						Expect(resp.RType).Should(Equal(model.ResponseTypeSYNTHESIZED))
						Expect(resp.Res.Rcode).Should(Equal(dns.RcodeSuccess))
						Expect(resp.Res.Answer).Should(HaveLen(1))
					}

					Expect(asked).Should(Equal(map[uint16]int{dns.TypeAAAA: 1, dns.TypeA: 1, dns.TypeDNSKEY: 1}))
				})

				It("answers SERVFAIL for an A RRset that is not authentic", func() {
					replies = []*dns.Msg{{Answer: []dns.RR{a}}}

					Expect(told(synthesized()).InfoCode).Should(Equal(dns.ExtendedErrorCodeRRSIGsMissing))
					Expect(asked[dns.TypeAAAA]).Should(Equal(1))
					Expect(asked[dns.TypeA]).Should(Equal(2), "asked again")
				})
			})

			DescribeTable("does not retry or cache exhausted validation work",
				func(second bool) {
					const links = 128
					name := func(i int) string { return fmt.Sprintf("h%d.example.com.", i) }
					chain := signed(newRR(name(links) + " 300 IN A 192.0.2.1"))
					for i := range links {
						chain = append(chain, signed(newRR(name(i)+" 300 IN CNAME "+name(i+1)))...)
					}
					replies = []*dns.Msg{{Answer: chain}}
					want := 1
					if second {
						forged := []dns.RR{newRR(name(0) + " 300 IN CNAME elsewhere.example.com."), chain[3]}
						replies = append([]*dns.Msg{{Answer: forged}}, replies...)
						want = 2
					}
					request.Req = util.NewMsgWithQuestion(name(0), A)
					Expect(told(resolve()).ExtraText).Should(ContainSubstring("more signature verifications"))
					Expect(asked[dns.TypeA]).Should(Equal(want))
					Expect(sut.failureCache.TotalCount()).Should(BeZero())
					Expect(told(resolve()).ExtraText).Should(ContainSubstring("more signature verifications"))
					Expect(asked[dns.TypeA]).Should(Equal(want + 1))
				}, Entry("first validation", false), Entry("second validation shares the first budget", true))

			It("does not retry or cache an exhausted upstream query budget", func() {
				sutConfig.MaxUpstreamQueries = 1
				frugal, err := NewDNSSECResolver(ctx, sutConfig, mockUpstream)
				Expect(err).Should(Succeed())
				frugal.Next(mockUpstream)
				replies = []*dns.Msg{{Answer: []dns.RR{newRR("www.unsigned.example.com. 300 IN A 192.0.2.1")}}}
				request.Req = util.NewMsgWithQuestion("www.unsigned.example.com.", A)
				resp, err := frugal.Resolve(ctx, request)
				Expect(err).Should(Succeed())
				Expect(told(resp).ExtraText).Should(ContainSubstring("no upstream query left"))
				Expect(asked[dns.TypeA]).Should(Equal(1))
				Expect(frugal.(*DNSSECResolver).failureCache.TotalCount()).Should(BeZero())
			})

			DescribeTable("asks once for data that validation accepts",
				func(name string, authentic bool) {
					if name != "example.com." {
						replies = []*dns.Msg{{Answer: []dns.RR{newRR(name + " 300 IN A 192.0.2.1")}}}
					}
					request.Req = util.NewMsgWithQuestion(name, A)
					resp := resolve()
					Expect(resp.Res.Rcode).Should(Equal(dns.RcodeSuccess))
					Expect(resp.Res.Answer).ShouldNot(BeEmpty())
					Expect(resp.Res.AuthenticatedData).Should(Equal(authentic))
					Expect(asked[dns.TypeA]).Should(Equal(1))
				}, Entry("Secure", "example.com.", true),
				Entry("Insecure", "www.unsigned.example.com.", false), Entry("Indeterminate", "example.org.", false))
		})

		It("should handle Indeterminate validation result", func() {
			req := util.NewMsgWithQuestion("example.com.", A)
			request := &model.Request{
				Req:      req,
				Protocol: model.RequestProtocolUDP,
			}

			// Response that will be indeterminate
			testResponse, _ := util.NewMsgWithAnswer("example.com.", 300, A, "192.0.2.1")
			testResponse.AuthenticatedData = true

			mockUpstream.On("Resolve", mock.Anything).Return(&model.Response{Res: testResponse}, nil)

			resp, err := sut.Resolve(ctx, request)
			Expect(err).Should(Succeed())
			Expect(resp.Res.AuthenticatedData).Should(BeFalse())
		})
	})
})
