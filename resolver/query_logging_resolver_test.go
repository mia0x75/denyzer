package resolver

import (
	"context"
	"os"
	"path/filepath"
	"time"

	. "github.com/mia0x75/denyzer/helpertest"
	"github.com/mia0x75/denyzer/log"
	"github.com/mia0x75/denyzer/querylog"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/mock"

	"github.com/mia0x75/denyzer/cache/stringcache"
	"github.com/mia0x75/denyzer/config"
	. "github.com/mia0x75/denyzer/model"

	"github.com/miekg/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type SlowMockWriter struct {
	entries []*querylog.LogEntry
}

func (m *SlowMockWriter) Write(entry *querylog.LogEntry) {
	m.entries = append(m.entries, entry)

	time.Sleep(100 * time.Millisecond)
}

func (m *SlowMockWriter) CleanUp() {
}

var _ = Describe("QueryLoggingResolver", func() {
	var (
		sut        *QueryLoggingResolver
		sutConfig  config.QueryLog
		err        error
		m          *mockResolver
		mockRType  ResponseType
		mockAnswer *dns.Msg

		ctx      context.Context
		cancelFn context.CancelFunc
	)

	Describe("Type", func() {
		It("follows conventions", func() {
			expectValidResolverType(sut)
		})
	})

	BeforeEach(func() {
		ctx, cancelFn = context.WithCancel(context.Background())
		DeferCleanup(cancelFn)

		sutConfig, err = config.WithDefaults[config.QueryLog]()
		Expect(err).Should(Succeed())

		mockRType = ResponseTypeRESOLVED
		mockAnswer = new(dns.Msg)
	})

	JustBeforeEach(func() {
		if len(sutConfig.Fields) == 0 {
			sutConfig.SetDefaults() // not called when using a struct literal
		}

		sut, err = NewQueryLoggingResolver(ctx, sutConfig)
		Expect(err).Should(Succeed())

		m = &mockResolver{
			ResolveFn: func(context.Context, *Request) (*Response, error) {
				return &Response{RType: mockRType, Res: mockAnswer, Reason: "reason"}, nil
			},
		}

		m.On("Resolve", mock.Anything).Return(autoAnswer, nil)

		sut.Next(m)
	})

	Describe("IsEnabled", func() {
		It("is true", func() {
			Expect(sut.IsEnabled()).Should(BeTrue())
		})
	})

	Describe("LogConfig", func() {
		It("should log something", func() {
			logger, hook := log.NewMockEntry()

			sut.LogConfig(logger)

			Expect(hook.Calls).ShouldNot(BeEmpty())
		})
	})

	Describe("Process request", func() {
		When("Resolver has no configuration", func() {
			BeforeEach(func() {
				sutConfig = config.QueryLog{
					CreationAttempts: 1,
					CreationCooldown: config.Duration(time.Millisecond),
				}
			})
			It("should process request without query logging", func() {
				Expect(sut.writer).Should(BeAssignableToTypeOf(&querylog.LoggerWriter{}))

				Expect(sut.Resolve(ctx, newRequest("example.com", A))).
					Should(
						SatisfyAll(
							HaveResponseType(ResponseTypeRESOLVED),
							HaveReturnCode(dns.RcodeSuccess),
						))

				m.AssertExpectations(GinkgoT())
			})
		})

		When("type is none", func() {
			BeforeEach(func() {
				sutConfig = config.QueryLog{
					Type:             config.QueryLogTypeNone,
					CreationAttempts: 1,
					CreationCooldown: config.Duration(time.Millisecond),
				}
			})
			It("should pass the request through without building a log entry", func() {
				Expect(sut.Resolve(ctx, newRequestWithClient("example.com.", A, "192.168.178.25", "client1"))).
					Should(
						SatisfyAll(
							HaveResponseType(ResponseTypeRESOLVED),
							HaveReturnCode(dns.RcodeSuccess),
						))

				m.AssertExpectations(GinkgoT())
			})
		})

		Describe("ignore", func() {
			var ignored *log.MockLoggerHook

			JustBeforeEach(func() {
				// Stop background goroutines
				cancelFn()

				ctx, cancelFn = context.WithCancel(context.Background())
				DeferCleanup(cancelFn)

				// Capture ignored logs
				{
					var logger *logrus.Entry

					logger, ignored = log.NewMockEntry()
					ctx, _ = log.NewCtx(ctx, logger)
				}
			})

			Describe("SUDN", func() {
				JustBeforeEach(func() {
					sut.cfg.Ignore.SUDN = true
				})

				It("should not log SUDN responses", func() {
					mockRType = ResponseTypeSPECIAL

					_, err := sut.Resolve(ctx, newRequestWithClient("example.com.", A, "192.168.178.25", "client1"))
					Expect(err).Should(Succeed())

					Expect(ignored.Calls).Should(HaveLen(1))
					Expect(ignored.Messages).Should(ContainElement(ContainSubstring("ignored querylog entry")))
				})

				It("should log other responses", func() {
					mockRType = ResponseTypeBLOCKED

					_, err := sut.Resolve(ctx, newRequestWithClient("example.com.", A, "192.168.178.25", "client1"))
					Expect(err).Should(Succeed())

					Expect(ignored.Calls).Should(BeEmpty())
				})
			})

			Describe("Domains", func() {
				BeforeEach(func() {
					sutConfig.Ignore.Domains = []string{"ignored.example.com", "*.noisy.lan"}
				})

				It("should not log a matching exact domain", func() {
					_, err := sut.Resolve(ctx,
						newRequestWithClient("ignored.example.com.", A, "192.168.178.25", "client1"))
					Expect(err).Should(Succeed())

					Expect(ignored.Messages).Should(ContainElement(ContainSubstring("ignored querylog entry")))
				})

				It("should not log a matching wildcard domain", func() {
					_, err := sut.Resolve(ctx,
						newRequestWithClient("host.noisy.lan.", A, "192.168.178.25", "client1"))
					Expect(err).Should(Succeed())

					Expect(ignored.Messages).Should(ContainElement(ContainSubstring("ignored querylog entry")))
				})

				It("should log a non-matching domain", func() {
					_, err := sut.Resolve(ctx,
						newRequestWithClient("example.com.", A, "192.168.178.25", "client1"))
					Expect(err).Should(Succeed())

					Expect(ignored.Calls).Should(BeEmpty())
				})
			})
		})
	})

	Describe("Clean up of query log", func() {
		When("fallback logger is enabled, log retention is enabled", func() {
			BeforeEach(func() {
				sutConfig = config.QueryLog{
					Retention:        config.QueryLogRetention{HistoryMonths: 7},
					Type:             config.QueryLogTypeConsole,
					CreationAttempts: 1,
					CreationCooldown: config.Duration(time.Millisecond),
				}
			})
			It("should do nothing", func() {
				sut.doCleanUp()
			})
		})
	})

	Describe("GetQueryLoggingWriter", func() {
		It("creates an async log writer when the type is omitted", func() {
			dbPath := filepath.Join(GinkgoT().TempDir(), "querylog.db")

			writer, err := GetQueryLoggingWriter(ctx, config.QueryLog{
				Target:        config.Secret(dbPath),
				FlushInterval: config.Duration(time.Millisecond),
			}, "test-instance")

			Expect(err).Should(Succeed())
			Expect(writer).Should(BeAssignableToTypeOf(&querylog.AsyncLogWriter{}))
		})

		It("creates an async log writer for sqlite type", func() {
			dbPath := filepath.Join(GinkgoT().TempDir(), "querylog.db")

			writer, err := GetQueryLoggingWriter(ctx, config.QueryLog{
				Type:          config.QueryLogTypeSqlite,
				Target:        config.Secret(dbPath),
				FlushInterval: config.Duration(time.Millisecond),
			}, "test-instance")

			Expect(err).Should(Succeed())
			Expect(writer).ShouldNot(BeNil())
			Expect(writer).Should(BeAssignableToTypeOf(&querylog.AsyncLogWriter{}))
		})
	})

	Describe("Hostname function tests", func() {
		It("should use the given file if it exists", func() {
			expected := "TestName"
			tmpFile := NewTmpFolder("hostname").CreateStringFile("filetest1", expected+" \n")

			Expect(readInstanceID(tmpFile.Path)).Should(Equal(expected))
		})

		It("should fallback to os.Hostname", func() {
			expected, err := os.Hostname()
			Expect(err).Should(Succeed())

			Expect(readInstanceID("/var/empty/nonexistent")).Should(Equal(expected))
		})
	})

	Describe("newIgnoreDomainsMatcher", func() {
		It("returns nil for an empty list", func() {
			logger, _ := log.NewMockEntry()
			Expect(newIgnoreDomainsMatcher(nil, logger)).Should(BeNil())
		})

		It("matches exact, wildcard and regex entries", func() {
			logger, _ := log.NewMockEntry()
			matcher := newIgnoreDomainsMatcher(
				[]string{"example.com", "*.lan", "/\\.arpa$/"}, logger)
			Expect(matcher).ShouldNot(BeNil())

			groups := []string{queryLogIgnoreGroup}
			Expect(matcher.Contains("example.com", groups)).ShouldNot(BeEmpty())
			Expect(matcher.Contains("host.lan", groups)).ShouldNot(BeEmpty())
			Expect(matcher.Contains("1.0.0.10.in-addr.arpa", groups)).ShouldNot(BeEmpty())
			Expect(matcher.Contains("other.com", groups)).Should(BeEmpty())
		})

		It("warns and skips an invalid entry but keeps valid ones", func() {
			logger, hook := log.NewMockEntry()
			matcher := newIgnoreDomainsMatcher(
				[]string{"/[invalid(/", "example.com"}, logger)

			Expect(matcher).ShouldNot(BeNil())
			Expect(matcher.Contains("example.com", []string{queryLogIgnoreGroup})).ShouldNot(BeEmpty())
			Expect(hook.Messages).Should(ContainElement(ContainSubstring("invalid")))
		})

		It("warns and skips a lone slash entry without panicking", func() {
			logger, hook := log.NewMockEntry()

			var matcher stringcache.GroupedStringCache
			Expect(func() {
				matcher = newIgnoreDomainsMatcher([]string{"/", "example.com"}, logger)
			}).ShouldNot(Panic())

			Expect(matcher.Contains("example.com", []string{queryLogIgnoreGroup})).ShouldNot(BeEmpty())
			Expect(hook.Messages).Should(ContainElement(ContainSubstring("invalid")))
		})

		It("warns and skips empty regex entries instead of matching every domain", func() {
			logger, hook := log.NewMockEntry()

			matcher := newIgnoreDomainsMatcher([]string{"//", "/ /", "example.com"}, logger)
			Expect(matcher).ShouldNot(BeNil())

			groups := []string{queryLogIgnoreGroup}
			// an empty regex would match everything; the entries must be rejected
			Expect(matcher.Contains("unrelated.example.net", groups)).Should(BeEmpty())
			Expect(matcher.Contains("example.com", groups)).ShouldNot(BeEmpty())
			Expect(hook.Messages).Should(ContainElement(ContainSubstring("invalid")))
		})
	})
})
