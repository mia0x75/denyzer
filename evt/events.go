package evt

import "github.com/asaskevich/EventBus"

const (
	// BlockingCacheGroupChanged fires, if a list group is changed. Parameter: list type, group name, element count
	BlockingCacheGroupChanged = "blocking:cachingGroupChanged"

	// CachingResultCacheChanged fires if a result cache was changed, Parameter: new cache size
	CachingResultCacheChanged = "caching:resultCacheChanged"

	// CachingDomainPrefetched fires after a domain was prefetched. Parameter: domain name
	CachingDomainPrefetched = "caching:domainPrefetched"

	// CachingPrefetchCacheHit fires if a domain was served from the prefetch cache. Parameter: domain name
	CachingPrefetchCacheHit = "caching:prefetchCacheHit"

	// CachingFailedDownloadChanged fires, if a download of a blocking list or hosts file fails
	CachingFailedDownloadChanged = "caching:failedDownload"

	// ApplicationStarted fires on start of the application. Parameter: version number, build time
	ApplicationStarted = "application:started"
)

//nolint:gochecknoglobals
var evtBus = EventBus.New()

// Bus returns the global bus instance
func Bus() EventBus.Bus {
	return evtBus
}
