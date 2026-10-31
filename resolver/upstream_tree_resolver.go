package resolver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/model"
	"github.com/mia0x75/denyzer/util"
	"github.com/sirupsen/logrus"
)

const (
	upstreamTreeResolverType = "upstream_tree"
)

type UpstreamTreeResolver struct {
	configurable[*config.Upstreams]
	typed

	branches map[string]Resolver
}

func NewUpstreamTreeResolver(ctx context.Context, cfg config.Upstreams, bootstrap *Bootstrap) (Resolver, error) {
	if len(cfg.Groups[upstreamDefaultCfgName]) == 0 {
		return nil, fmt.Errorf("no external DNS resolvers configured as default upstream resolvers. "+
			"Please configure at least one under '%s' configuration name", upstreamDefaultCfgName)
	}

	branches, err := createUpstreamBranches(ctx, cfg, bootstrap)
	if err != nil {
		return nil, err
	}

	if len(branches) == 1 {
		for _, r := range branches {
			return r, nil
		}
	}

	// return resolver that forwards request to specific resolver branch depending on the client
	r := UpstreamTreeResolver{
		configurable: withConfig(&cfg),
		typed:        withType(upstreamTreeResolverType),

		branches: branches,
	}

	return &r, nil
}

func createUpstreamBranches(
	ctx context.Context, cfg config.Upstreams, bootstrap *Bootstrap,
) (map[string]Resolver, error) {
	// Each branch verifies its upstreams with a test query, so building them one
	// after the other makes startup the sum of every group's latency. The groups
	// are independent of each other and are therefore initialised concurrently.
	groups := make([]string, 0, len(cfg.Groups))
	groupUpstreams := make([][]config.Upstream, 0, len(cfg.Groups))

	for group, upstreams := range cfg.Groups {
		groups = append(groups, group)
		groupUpstreams = append(groupUpstreams, upstreams)
	}

	resolvers := make([]Resolver, len(groups))
	errs := make([]error, len(groups))

	var wg sync.WaitGroup

	wg.Add(len(groups))

	for i := range groups {
		go func() {
			defer wg.Done()

			resolvers[i], errs[i] = newUpstreamGroupResolver(ctx, cfg, groups[i], groupUpstreams[i], bootstrap)
		}()
	}

	wg.Wait()

	branches := make(map[string]Resolver, len(groups))
	joinErrs := make([]error, 0, len(groups))

	for i, group := range groups {
		if errs[i] != nil {
			joinErrs = append(joinErrs, errs[i])

			continue
		}

		branches[group] = resolvers[i]
	}

	if len(joinErrs) != 0 {
		return nil, errors.Join(joinErrs...)
	}

	return branches, nil
}

func newUpstreamGroupResolver(
	ctx context.Context, cfg config.Upstreams, group string, upstreams []config.Upstream, bootstrap *Bootstrap,
) (Resolver, error) {
	groupConfig := config.NewUpstreamGroup(group, cfg, upstreams)

	var (
		resolver Resolver
		err      error
	)

	switch cfg.Strategy {
	case config.UpstreamStrategyParallelBest, config.UpstreamStrategyRandom:
		resolver, err = NewParallelBestResolver(ctx, groupConfig, bootstrap)
	case config.UpstreamStrategyStrict:
		resolver, err = NewStrictResolver(ctx, groupConfig, bootstrap)
	}

	if err != nil {
		return nil, fmt.Errorf("group %s: %w", group, err)
	}

	return resolver, nil
}

func (r *UpstreamTreeResolver) Name() string {
	return r.String()
}

func (r *UpstreamTreeResolver) String() string {
	result := make([]string, 0, len(r.branches))

	for group, res := range r.branches {
		result = append(result, fmt.Sprintf("%s (%s)", group, res.Type()))
	}

	return fmt.Sprintf("%s upstreams %q", upstreamTreeResolverType, strings.Join(result, ", "))
}

func (r *UpstreamTreeResolver) Resolve(ctx context.Context, request *model.Request) (*model.Response, error) {
	ctx, logger := r.log(ctx)

	group := r.upstreamGroupByClient(logger, request)

	// delegate request to group resolver
	logger.WithField("resolver", fmt.Sprintf("%s (%s)", group, r.branches[group].Type())).Debug("delegating to resolver")

	resp, err := r.branches[group].Resolve(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("upstream resolution failed for group '%s': %w", group, err)
	}

	return resp, nil
}

func (r *UpstreamTreeResolver) upstreamGroupByClient(logger *logrus.Entry, request *model.Request) string {
	groups := make([]string, 0, len(r.branches))
	clientIP := request.ClientIP.String()

	// try IP
	if _, exists := r.branches[clientIP]; exists {
		return clientIP
	}

	// try client names
	for _, name := range request.ClientNames {
		for group := range r.branches {
			if util.ClientNameMatchesGroupName(group, name) {
				groups = append(groups, group)
			}
		}
	}

	// try CIDR (only if no client name matched)
	if len(groups) == 0 {
		for cidr := range r.branches {
			if util.CidrContainsIP(cidr, request.ClientIP) {
				groups = append(groups, cidr)
			}
		}
	}

	if len(groups) > 0 {
		if len(groups) > 1 {
			// Pick the same group for every request of the client: the DNSSEC resolver keys its
			// validation cache by this group, and its sub-queries must reach the same group.
			slices.Sort(groups)

			logger.WithFields(logrus.Fields{
				"clientNames": request.ClientNames,
				"clientIP":    clientIP,
				"groups":      groups,
			}).Warn("client matches multiple groups")
		}

		return groups[0]
	}

	return upstreamDefaultCfgName
}
