package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mia0x75/denyzer/helpertest"
	"github.com/mia0x75/denyzer/storage"
	"github.com/miekg/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	// basePort is the base port for the test server. It must not be used by
	// any other suite: suites can run in parallel, and they would otherwise
	// compete for the same port.
	basePort = 5250
)

var _ = Describe("Serve command", func() {
	var (
		tmpDir *helpertest.TmpFolder
		port   string
	)
	BeforeEach(func() {
		port = helpertest.GetStringPort(basePort)
		tmpDir = helpertest.NewTmpFolder("config")

		configPath = defaultConfigPath
	})

	When("Serve command is called with valid config", func() {
		It("finishes and flushes an in-flight query before cancelling on a signal", func() {
			upstreamConn, err := net.ListenPacket("udp", "127.0.0.1:0")
			Expect(err).Should(Succeed())
			DeferCleanup(func() { _ = upstreamConn.Close() })
			started := make(chan struct{})
			release := make(chan struct{}, 1)
			DeferCleanup(func() { close(release) })
			upstreamReady := make(chan struct{})
			const domain = "shutdown-regression.winnow.dev"
			upstream := &dns.Server{
				Net: "udp", PacketConn: upstreamConn,
				NotifyStartedFunc: func() { close(upstreamReady) },
				Handler: dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
					answer := new(dns.Msg)
					answer.SetReply(req)
					if req.Question[0].Name == dns.Fqdn(domain) {
						close(started)
						<-release
						answer.Answer = []dns.RR{&dns.A{
							Hdr: dns.RR_Header{Name: dns.Fqdn(domain), Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
							A:   net.ParseIP("203.0.113.1"),
						}}
					}
					_ = w.WriteMsg(answer)
				}),
			}
			upstreamResult := make(chan error, 1)
			go func() { upstreamResult <- upstream.ActivateAndServe() }()
			Eventually(upstreamReady).Should(BeClosed())
			DeferCleanup(func() {
				_ = upstream.Shutdown()
				Eventually(upstreamResult).Should(Receive(BeNil()))
			})
			cfgFile := tmpDir.CreateStringFile("config.yaml",
				"upstreams:", "  groups:", "    default:", "      - "+upstreamConn.LocalAddr().String(),
				"ports:", "  dns: "+port,
				"queryLog:", "  type: sqlite", "  target: "+filepath.Join(GinkgoT().TempDir(), "queries.db"),
				"  flushInterval: 1h", "  batchSize: 100")
			GinkgoT().Setenv(configFileEnvVar, cfgFile.Path)
			stopped := make(chan error, 1)
			finished := false
			go func() { stopped <- startServer(newServeCommand(), nil) }()
			DeferCleanup(func() {
				if !finished {
					select {
					case release <- struct{}{}:
					default:
					}
					select {
					case signals <- syscall.SIGINT:
					default:
					}
					Eventually(stopped, "12s").Should(Receive())
				}
			})
			Eventually(func(g Gomega) {
				conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond)
				g.Expect(err).Should(Succeed())
				_ = conn.Close()
			}, "5s").Should(Succeed())
			result := make(chan error, 1)
			queryDone := make(chan struct{})
			address := net.JoinHostPort("127.0.0.1", port)
			go func() {
				defer close(queryDone)
				client := &dns.Client{Net: "udp", Timeout: 5 * time.Second}
				answer, _, err := client.Exchange(new(dns.Msg).SetQuestion(dns.Fqdn(domain), dns.TypeA), address)
				if err == nil && answer.Rcode != dns.RcodeSuccess {
					err = fmt.Errorf("query returned rcode %d", answer.Rcode)
				}
				result <- err
			}()
			DeferCleanup(func() {
				select {
				case release <- struct{}{}:
				default:
				}
				Eventually(queryDone, "6s").Should(BeClosed())
			})
			Eventually(started).Should(BeClosed())
			signals <- syscall.SIGINT
			Consistently(result, "50ms").Should(BeEmpty())
			release <- struct{}{}
			Eventually(result).Should(Receive(BeNil()))
			Eventually(stopped, "5s").Should(Receive(BeNil()))
			finished = true
			page, err := storage.GetQueryLogsPage(context.Background(), "", 10, domain, "", "", "")
			Expect(err).Should(Succeed())
			Expect(page.Entries).Should(HaveLen(1))
			Expect(page.Entries[0].ResponseType).Should(Equal("RESOLVED"))
		})

		It("should start without error and terminate with signal", func() {
			By("initialize config", func() {
				cfgFile := tmpDir.CreateStringFile("config.yaml",
					"upstreams:",
					"  groups:",
					"    default:",
					"      - 1.1.1.1",
					"ports:",
					"  dns: "+port)

				os.Setenv(configFileEnvVar, cfgFile.Path)
				DeferCleanup(func() { os.Unsetenv(configFileEnvVar) })
			})

			errChan := make(chan error)
			By("start server", func() {
				go func() {
					// it is a blocking function, call async
					errChan <- startServer(newServeCommand(), []string{})
				}()
			})

			By("check DNS port is open", func() {
				Eventually(func(g Gomega) {
					conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond)
					g.Expect(err).Should(Succeed())
					defer conn.Close()
				}, "5s").Should(Succeed())
			})

			By("terminate with signal", func() {
				signals <- syscall.SIGINT

				// no errors
				Eventually(errChan).Should(Receive(BeNil()))
			})
		})
	})

	When("Serve command is called with valid config", func() {
		It("should fail if server start fails", func() {
			listener, err := net.Listen("tcp", "127.0.0.1:"+port)
			Expect(err).Should(Succeed())
			DeferCleanup(func() { _ = listener.Close() })
			dnsPort := helpertest.GetStringPort(basePort + 1)
			By("initialize config with blocked port "+port, func() {
				cfgFile := tmpDir.CreateStringFile("config.yaml",
					"upstreams:",
					"  groups:",
					"    default:",
					"      - 1.1.1.1",
					"ports:",
					"  dns: "+dnsPort,
					"  http: "+port)

				os.Setenv(configFileEnvVar, cfgFile.Path)
				DeferCleanup(func() { os.Unsetenv(configFileEnvVar) })
			})

			errChan := make(chan error)
			By("start server", func() {
				go func() {
					// it is a blocking function, call async
					errChan <- startServer(newServeCommand(), []string{})
				}()
			})

			By("terminate with signal", func() {
				var startError error
				Eventually(errChan, "10s").Should(Receive(&startError))
				Expect(startError).Should(MatchError(ContainSubstring("address already in use")))
			})
			By("releasing DNS listeners after the later HTTP bind fails", func() {
				conn, err := net.Listen("tcp", "127.0.0.1:"+dnsPort)
				Expect(err).Should(Succeed())
				DeferCleanup(func() { _ = conn.Close() })
				packetConn, err := net.ListenPacket("udp", "127.0.0.1:"+dnsPort)
				Expect(err).Should(Succeed())
				DeferCleanup(func() { _ = packetConn.Close() })
			})
		})
	})

	When("Serve command is called without config", func() {
		It("should fail to start and report error", func() {
			// startServer takes the config path from the environment, so none may be set
			for _, name := range []string{configFileEnvVar, configFileEnvVarOld} {
				if val, ok := os.LookupEnv(name); ok {
					Expect(os.Unsetenv(name)).Should(Succeed())
					DeferCleanup(os.Setenv, name, val)
				}
			}

			errChan := make(chan error)
			By("start server", func() {
				go func() {
					// it is a blocking function, call async
					errChan <- startServer(newServeCommand(), []string{})
				}()
			})

			By("server should terminate with error", func() {
				var startError error
				Eventually(errChan).Should(Receive(&startError))
				Expect(startError).Should(MatchError(ContainSubstring("unable to load configuration")))
			})
		})
	})
})
