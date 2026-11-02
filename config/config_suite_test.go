package config

import (
	"log/slog"
	"testing"

	"github.com/mia0x75/denyzer/log"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	logger *slog.Logger
	hook   *log.Recorder
)

func init() {
	log.Silence()
}

func TestConfig(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Config Suite")
}

func suiteBeforeEach() {
	BeforeEach(func() {
		logger, hook = log.NewMockLogger()
	})
}
