package stringcache_test

import (
	"testing"

	"github.com/mia0x75/denyzer/log"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func init() {
	log.Silence()
}

func TestCache(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "String cache suite")
}
