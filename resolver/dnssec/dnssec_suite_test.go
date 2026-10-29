package dnssec

import (
	"testing"

	"github.com/mia0x75/denyzer/log"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func init() {
	log.Silence()
}

func TestDNSSEC(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "DNSSEC Suite")
}
