package cmd

import (
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/mia0x75/denyzer/api"
	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/log"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "github.com/mia0x75/denyzer/helpertest"
)

// Mock implementation of codeWithStatus interface for testing
type mockResponse struct {
	statusCode int
	status     string
}

func (m mockResponse) StatusCode() int {
	return m.statusCode
}

func (m mockResponse) Status() string {
	return m.status
}

var _ = Describe("root command", func() {
	When("Version command is called", func() {
		It("should execute without error", func() {
			c := NewRootCommand()
			c.SetOut(io.Discard)
			c.SetArgs([]string{"help"})
			err := c.Execute()
			Expect(err).Should(Succeed())
		})
	})

	When("Config provided", func() {
		var (
			tmpDir        *TmpFolder
			tmpFile       *TmpFile
			dohOnlyConfig *TmpFile
		)

		BeforeEach(func() {
			configPath = defaultConfigPath

			tmpDir = NewTmpFolder("RootCommand")
			tmpFile = tmpDir.CreateStringFile("config",
				"upstreams:",
				"  groups:",
				"    default:",
				"      - 1.1.1.1",
				"blocking:",
				"  denylists:",
				"    ads:",
				"      - https://s3.amazonaws.com/lists.disconnect.me/simple_ad.txt",
				"  clientGroupsBlock:",
				"    default:",
				"      - ads",
				"port: 5333",
			)
			dohOnlyConfig = tmpDir.CreateStringFile("config_without_admin",
				"upstreams:", "  groups:", "    default:", "      - 1.1.1.1",
				"ports:", "  http: 4000",
			)
		})

		It("rejects API commands when ports.admin is not configured", func() {
			keepGlobals()
			command := NewRootCommand()
			command.SetArgs([]string{"refresh", "--config", dohOnlyConfig.Path})

			Expect(command.Execute()).Should(MatchError("ports.admin is not configured"))
		})

		It("allows an explicit API address without ports.admin", func() {
			keepGlobals()
			command := NewRootCommand()
			server := testHTTPAPIServer(func(http.ResponseWriter, *http.Request) {})
			DeferCleanup(server.Close)
			command.SetArgs([]string{
				"refresh", "--config", dohOnlyConfig.Path,
				"--apiHost", apiHost, "--apiPort", strconv.Itoa(int(apiPort)),
			})

			Expect(command.Execute()).Should(Succeed())
		})

		It("should accept old env var", func() {
			os.Setenv(configFileEnvVarOld, tmpFile.Path)
			DeferCleanup(func() { os.Unsetenv(configFileEnvVarOld) })

			_, err := initConfig()
			Expect(err).Should(Succeed())

			Expect(configPath).Should(Equal(tmpFile.Path))
		})

		It("should accept new env var", func() {
			os.Setenv(configFileEnvVar, tmpFile.Path)
			DeferCleanup(func() { os.Unsetenv(configFileEnvVar) })

			_, err := initConfig()
			Expect(err).Should(Succeed())

			Expect(configPath).Should(Equal(tmpFile.Path))
		})

		It("should keep command output in the terminal when the server logs to syslog", func() {
			syslogConfig := tmpDir.CreateStringFile("config_with_syslog",
				"upstreams:",
				"  groups:",
				"    default:",
				"      - 1.1.1.1",
				"log:",
				"  target: syslog",
				"  syslog:",
				"    network: udp",
				"    address: 127.0.0.1:1",
			)

			configPath = syslogConfig.Path

			_, err := initConfig()
			Expect(err).Should(Succeed())
			logCfg := commandLogConfig(&config.Config{Log: log.Config{Target: log.TargetTypeSyslog}})
			Expect(logCfg.Target).Should(Equal(log.TargetTypeStdout))
		})

		It("should handle config with management port", func() {
			configWithAdmin := tmpDir.CreateStringFile("config_with_admin",
				"upstreams:",
				"  groups:",
				"    default:",
				"      - 1.1.1.1",
				"ports:",
				"  admin:",
				"    - 127.0.0.1:8080",
			)

			configPath = configWithAdmin.Path

			_, err := initConfig()
			Expect(err).Should(Succeed())
			Expect(apiHost).Should(Equal("127.0.0.1"))
			Expect(apiPort).Should(Equal(uint16(8080)))
		})

		DescribeTable("explicit management flags override configuration", func(flags []string, expectedURL string) {
			configWithAdmin := tmpDir.CreateStringFile("config_with_admin",
				"upstreams:", "  groups:", "    default:", "      - 1.1.1.1",
				"ports:", "  admin: 127.0.0.1:8080",
			)
			command := NewRootCommand()
			Expect(command.ParseFlags(append([]string{"--config", configWithAdmin.Path}, flags...))).Should(Succeed())
			Expect(initConfigPreRun(command, nil)).Should(Succeed())
			Expect(apiURL()).Should(Equal(expectedURL))
		},
			Entry("defaults use config", []string{}, "https://127.0.0.1:8080/api"),
			Entry("certificate hostname", []string{"--apiHost", "admin.example"}, "https://admin.example:8080/api"),
			Entry("explicit default hostname", []string{"--apiHost", "localhost"}, "https://localhost:8080/api"),
			Entry("explicit port", []string{"--apiPort", "8443"}, "https://127.0.0.1:8443/api"),
			Entry("both flags", []string{"--apiHost", "admin.example", "--apiPort", "8443"}, "https://admin.example:8443/api"),
		)

		It("should handle config with invalid management port", func() {
			configWithInvalidAdmin := tmpDir.CreateStringFile("config_with_invalid_admin",
				"upstreams:",
				"  groups:",
				"    default:",
				"      - 1.1.1.1",
				"ports:",
				"  admin:",
				"    - 127.0.0.1:invalid",
			)

			configPath = configWithInvalidAdmin.Path

			_, err := initConfig()
			Expect(err).Should(HaveOccurred())
			Expect(err.Error()).Should(ContainSubstring("can't convert port"))
		})
	})

	Describe("apiURL function", func() {
		It("should return correct URL with default values", func() {
			apiHost = defaultHost
			apiPort = defaultPort

			url := apiURL()
			Expect(url).Should(Equal("https://localhost:4000/api"))
		})

		It("should return correct URL with custom values", func() {
			apiHost = "127.0.0.1"
			apiPort = 8080

			url := apiURL()
			Expect(url).Should(Equal("https://127.0.0.1:8080/api"))
		})
	})

	DescribeTable("HTTPS management clients share Bearer authorization", func(token, expected string) {
		GinkgoT().Setenv("DENYZER_API_TOKEN", token)
		requests := make(chan *http.Request, 2)
		ts := testHTTPAPIServer(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			requests <- r
			writeTestJSON(w, api.ApiBlockingStatus{Enabled: true})
		})
		DeferCleanup(ts.Close)
		client, err := newAPIClient()
		Expect(err).Should(Succeed())
		response, err := client.BlockingStatusWithResponse(GinkgoT().Context())
		Expect(err).Should(Succeed())
		Expect(response.StatusCode()).Should(Equal(http.StatusOK))
		var summary statsSummary
		Expect(getJSON(GinkgoT().Context(), "/api/stats/summary", nil, &summary)).Should(Succeed())
		for range 2 {
			request := <-requests
			Expect(request.TLS).ShouldNot(BeNil())
			Expect(request.Header.Get("Authorization")).Should(Equal(expected))
		}
	}, Entry("with a token", "test-only-token", "Bearer test-only-token"), Entry("without a token", "", ""))

	It("does not skip management certificate verification", func() {
		ts := testHTTPAPIServer(func(_ http.ResponseWriter, _ *http.Request) {
			defer GinkgoRecover()
			Fail("an untrusted TLS connection must not reach the API")
		})
		DeferCleanup(ts.Close)
		transport := &http.Transport{}
		DeferCleanup(transport.CloseIdleConnections)
		http.DefaultTransport = transport
		client, err := newAPIClient()
		Expect(err).Should(Succeed())
		_, err = client.BlockingStatusWithResponse(GinkgoT().Context())
		Expect(err).Should(MatchError(ContainSubstring("certificate signed by unknown authority")))
		var summary statsSummary
		Expect(getJSON(GinkgoT().Context(), "/api/stats/summary", nil, &summary)).Should(
			MatchError(ContainSubstring("certificate signed by unknown authority")))
	})

	Describe("printOkOrError function", func() {
		It("should return nil for OK status", func() {
			resp := mockResponse{
				statusCode: http.StatusOK,
				status:     "200 OK",
			}

			err := printOkOrError(resp, "")
			Expect(err).ShouldNot(HaveOccurred())
		})

		It("should return error for non-OK status", func() {
			resp := mockResponse{
				statusCode: http.StatusBadRequest,
				status:     "400 Bad Request",
			}

			err := printOkOrError(resp, "Error message")
			Expect(err).Should(HaveOccurred())
			Expect(err.Error()).Should(ContainSubstring("400 Bad Request"))
			Expect(err.Error()).Should(ContainSubstring("Error message"))
		})
	})

	Describe("Command execution", func() {
		BeforeEach(func() {
			// Reset to default values before each test
			configPath = defaultConfigPath
			apiHost = defaultHost
			apiPort = defaultPort
		})

		It("should create root command with all subcommands", func() {
			cmd := NewRootCommand()

			// Check if all subcommands are added
			subCmdNames := make([]string, 0, len(cmd.Commands()))
			for _, subCmd := range cmd.Commands() {
				subCmdNames = append(subCmdNames, subCmd.Name())
			}

			expectedCmds := []string{
				"refresh", "query", "version", "serve",
				"blocking", "lists", "healthcheck", "cache", "validate", "auth",
			}
			for _, expected := range expectedCmds {
				Expect(subCmdNames).Should(ContainElement(expected))
			}
		})

		It("should set flags correctly", func() {
			cmd := NewRootCommand()

			// Test config flag
			configFlag := cmd.PersistentFlags().Lookup("config")
			Expect(configFlag).ShouldNot(BeNil())
			Expect(configFlag.Shorthand).Should(Equal("c"))
			Expect(configFlag.DefValue).Should(Equal(defaultConfigPath))

			// Test apiHost flag
			apiHostFlag := cmd.PersistentFlags().Lookup("apiHost")
			Expect(apiHostFlag).ShouldNot(BeNil())
			Expect(apiHostFlag.DefValue).Should(Equal(defaultHost))

			// Test apiPort flag
			apiPortFlag := cmd.PersistentFlags().Lookup("apiPort")
			Expect(apiPortFlag).ShouldNot(BeNil())
			Expect(apiPortFlag.DefValue).Should(Equal("4000"))
		})
	})
})
