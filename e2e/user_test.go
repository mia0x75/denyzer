package e2e

import (
	"context"
	"strconv"

	"github.com/moby/moby/api/types/container"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
)

var _ = Describe("Container user", func() {
	var e2eNet *testcontainers.DockerNetwork

	BeforeEach(func(ctx context.Context) {
		e2eNet = getRandomNetwork(ctx)
	})

	// Primary GID 0 must retain access to root-owned configs with mode 0640.
	DescribeTable("starts and passes healthcheck",
		func(ctx context.Context, user string, configMode int64) {
			req := buildDenyzerContainerRequest(createTempFile(
				"upstreams:", "  groups:", "    default:", "      - 127.0.0.1",
				"ports:", "  dns: "+strconv.Itoa(e2eDNSPort)))
			req.Files[0].FileMode = configMode

			baseConfigModifier := req.ConfigModifier
			req.ConfigModifier = func(c *container.Config) {
				baseConfigModifier(c)
				c.User = user
			}

			denyzer, err := startContainerWithNetwork(ctx, req, "denyzer", e2eNet)
			Expect(err).Should(Succeed())
			Expect(denyzer.IsRunning()).Should(BeTrue())
		},
		Entry("default image user", "", int64(0o640)),
		Entry("numeric UID", "100", int64(0o640)),
		Entry("named user", "denyzer", int64(0o640)),
		Entry("named user and group", "denyzer:denyzer", int64(modeWorldReadable)),
		Entry("numeric UID and GID", "100:100", int64(modeWorldReadable)),
	)
})
