package resolver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/mia0x75/denyzer/cache/stringcache"
	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/log"
	"github.com/mia0x75/denyzer/model"
	"github.com/mia0x75/denyzer/querylog"
	"github.com/mia0x75/denyzer/util"
	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

const (
	cleanUpRunPeriod         = 12 * time.Hour
	queryLoggingResolverType = "query_logging"
	defaultClientIP          = "0.0.0.0"
	queryLogIgnoreGroup      = "ignore"
)

// queryLogIgnoreGroups is the (single) cache group queried for ignore rules.
// Hoisted to avoid allocating a one-element slice on every resolved query.
var queryLogIgnoreGroups = []string{queryLogIgnoreGroup} //nolint:gochecknoglobals

// QueryLoggingResolver writes query information (question, answer, duration, ...)
type QueryLoggingResolver struct {
	configurable[*config.QueryLog]
	NextResolver
	typed

	writer        querylog.Writer
	instanceID    string
	ignoreDomains stringcache.GroupedStringCache
}

func GetQueryLoggingWriter(ctx context.Context, cfg config.QueryLog, instanceID string) (querylog.Writer, error) {
	var writer querylog.Writer

	var err error

	switch cfg.Type {
	case config.QueryLogTypeSqlite:
		writer, err = querylog.NewAsyncLogWriter(ctx, cfg.Target.Reveal(), cfg.Retention,
			cfg.FlushInterval.ToDuration(), cfg.BatchSize)
	case config.QueryLogTypeConsole:
		writer = querylog.NewLoggerWriter()
	case config.QueryLogTypeNone:
		writer = querylog.NewNoneWriter()
	}

	if err != nil {
		return nil, fmt.Errorf("failed to create query log writer (type: %s): %w", cfg.Type, err)
	}

	return writer, nil
}

// newIgnoreDomainsMatcher builds a matcher for the queryLog.ignore.domains rules.
// Returns nil when no domains are configured, so the per-query path stays free.
func newIgnoreDomainsMatcher(domains []string, logger *logrus.Entry) stringcache.GroupedStringCache {
	if len(domains) == 0 {
		return nil
	}

	matcher := stringcache.NewChainedGroupedCache(
		stringcache.NewInMemoryGroupedRegexCache(),
		stringcache.NewInMemoryGroupedWildcardCache(), // must follow regex (regex can contain '*')
		stringcache.NewInMemoryGroupedStringCache(),
	)

	factory := matcher.Refresh(queryLogIgnoreGroup)

	for _, d := range domains {
		// Pre-validate regex entries so we can warn via the caller's logger rather
		// than the global one (the underlying cache silently drops invalid regex,
		// and a lone "/" would panic the regex cache's unguarded slice).
		if strings.HasPrefix(d, "/") && strings.HasSuffix(d, "/") {
			// Strip the surrounding slashes. Reject entries with no actual pattern
			// (e.g. "/", "//", "/ /"): an empty regex matches every domain and would
			// silently drop the whole query log.
			inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(d, "/"), "/"))
			if inner == "" {
				logger.Warnf("ignoring invalid queryLog.ignore.domains entry: %q", d)

				continue
			}

			if _, err := regexp.Compile(inner); err != nil {
				logger.Warnf("ignoring invalid queryLog.ignore.domains entry: %q", d)

				continue
			}
		}

		factory.AddEntry(d)
	}

	factory.Finish()

	return matcher
}

// NewQueryLoggingResolver returns a new resolver instance
func NewQueryLoggingResolver(ctx context.Context, cfg config.QueryLog) (*QueryLoggingResolver, error) {
	logger := log.PrefixedLog(queryLoggingResolverType)

	var writer querylog.Writer

	instanceID, err := readInstanceID("/etc/hostname")
	if err != nil {
		return nil, fmt.Errorf("failed to determine instance ID for query logging: %w", err)
	}

	// queryLog.type defaults to sqlite, so an empty target means query logging
	// was not configured at all. Keep the console behaviour instead of retrying
	// against a database that has no path.
	if cfg.Type == config.QueryLogTypeSqlite && cfg.Target == "" {
		logger.Debug("no queryLog.target configured, logging to console")

		cfg.Type = config.QueryLogTypeConsole
	}

	err = retry.Do(
		func() error {
			var err error

			writer, err = GetQueryLoggingWriter(ctx, cfg, instanceID)

			return err
		},
		retry.Attempts(uint(cfg.CreationAttempts)),
		retry.DelayType(retry.FixedDelay),
		retry.Delay(cfg.CreationCooldown.ToDuration()),
		retry.OnRetry(func(n uint, err error) {
			logger.Warnf(
				"Error occurred on query writer creation, retry attempt %d/%d: %v", n+1, cfg.CreationAttempts, err,
			)
		}))
	if err != nil {
		logger.Error("can't create query log writer, using console as fallback: ", err)

		writer = querylog.NewLoggerWriter()
		cfg.Type = config.QueryLogTypeConsole
	}

	ignoreDomains := newIgnoreDomainsMatcher(cfg.Ignore.Domains, logger)

	resolver := QueryLoggingResolver{
		configurable: withConfig(&cfg),
		typed:        withType(queryLoggingResolverType),

		writer:        writer,
		instanceID:    instanceID,
		ignoreDomains: ignoreDomains,
	}

	// TTL cleanup is handled by AsyncLogWriter internally, but for non-sqlite
	// writers we still need a periodic cleanup goroutine.
	if cfg.Type != config.QueryLogTypeSqlite {
		go resolver.periodicCleanUp(ctx)
	}

	return &resolver, nil
}

// triggers periodically cleanup of old log files
func (r *QueryLoggingResolver) periodicCleanUp(ctx context.Context) {
	ticker := time.NewTicker(cleanUpRunPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			r.doCleanUp()
		case <-ctx.Done():
			r.closeWriter()

			return
		}
	}
}

func (r *QueryLoggingResolver) doCleanUp() {
	r.writer.CleanUp()
}

// Resolve logs the query, duration and the result
func (r *QueryLoggingResolver) Resolve(ctx context.Context, request *model.Request) (*model.Response, error) {
	// Query logging disabled: nothing this resolver does has any effect, so skip
	// the per-request work (logger derivation, entry building) entirely.
	if r.cfg.Type == config.QueryLogTypeNone {
		return r.next.Resolve(ctx, request)
	}

	ctx, logger := r.log(ctx)

	start := time.Now()
	resp, err := r.next.Resolve(ctx, request)
	duration := time.Since(start).Milliseconds()

	if err != nil {
		r.writer.RecordError()

		return nil, err
	}

	if r.ignore(request, resp) {
		// Build the entry only to explain (in the debug log) why it was ignored,
		// and only when that debug line would actually be emitted.
		if isDebugEnabled(logger) {
			if entry := r.createLogEntry(request, resp, start, duration); entry != nil {
				logger.WithFields(querylog.LogEntryFields(entry)).Debug("ignored querylog entry")
			}
		}

		return resp, nil
	}

	entry := r.createLogEntry(request, resp, start, duration)
	if entry == nil {
		return resp, nil
	}

	r.writer.Write(entry)

	return resp, nil
}

func (r *QueryLoggingResolver) ignore(request *model.Request, response *model.Response) bool {
	cfg := r.cfg.Ignore

	if cfg.SUDN && response.RType == model.ResponseTypeSPECIAL {
		return true
	}

	if r.ignoreDomains != nil {
		domain := util.ExtractDomain(request.Req.Question[0])
		if len(r.ignoreDomains.Contains(domain, queryLogIgnoreGroups)) > 0 {
			return true
		}
	}

	// If we add more ways to ignore entries, it would be nice to log why it's ignored in the debug log
	// Probably make this func return a (string, bool).

	return false
}

func (r *QueryLoggingResolver) createLogEntry(request *model.Request, response *model.Response,
	start time.Time, durationMs int64,
) *querylog.LogEntry {
	entry := querylog.LogEntry{
		Start:           start,
		ClientIP:        defaultClientIP,
		ClientNames:     []string{"none"},
		DenyzerInstance: r.instanceID,
	}

	for _, f := range r.cfg.Fields {
		switch f {
		case config.QueryLogFieldClientIP:
			entry.ClientIP = request.ClientIP.String()

		case config.QueryLogFieldClientName:
			entry.ClientNames = request.ClientNames

		case config.QueryLogFieldResponseReason:
			entry.ResponseReason = response.Reason
			entry.ResponseType = response.RType.String()
			entry.ResponseCode = dns.RcodeToString[response.Res.Rcode]

		case config.QueryLogFieldResponseAnswer:
			entry.Answer = util.Obfuscate(util.AnswerToString(response.Res.Answer))

		case config.QueryLogFieldQuestion:
			entry.QuestionName = util.Obfuscate(request.Req.Question[0].Name)
			entry.QuestionType = dns.TypeToString[request.Req.Question[0].Qtype]

		case config.QueryLogFieldDuration:
			entry.DurationMs = durationMs
		}
	}

	return &entry
}

// closeWriter lets writers that hold external resources (e.g. the dnstap socket)
// flush and release them on shutdown. Writers without a Close — the DB/CSV/console
// retention writers — are unaffected.
func (r *QueryLoggingResolver) closeWriter() {
	if c, ok := r.writer.(io.Closer); ok {
		if err := c.Close(); err != nil {
			log.PrefixedLog(queryLoggingResolverType).WithError(err).Warn("failed to close query log writer")
		}
	}
}

func readInstanceID(file string) (string, error) {
	// Prefer /etc/hostname over os.Hostname to allow easy differentiation in a Docker Swarm
	// See details in https://github.com/mia0x75/denyzer/pull/756
	hn, fErr := os.ReadFile(file)
	if fErr == nil {
		return strings.TrimSpace(string(hn)), nil
	}

	hostname, osErr := os.Hostname()
	if osErr == nil {
		return hostname, nil
	}

	return "", fmt.Errorf("cannot determine instance ID: %w", errors.Join(fErr, osErr))
}
