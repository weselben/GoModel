package virtualmodels

import (
	"context"

	"github.com/enterpilot/gomodel/internal/core"
)

// ResolveRepetitionLimit returns the request-scoped repetition-guard settings
// for a resolved model: the consecutive-repeat count that aborts the stream and
// the maximum token chain the guard treats as one repeating unit. A matching
// alias setting takes precedence over its concrete target; otherwise the normal
// exact/provider/model/global policy precedence applies. Each field
// independently inherits when nil, so an alias may set only the limit and let
// the max_pattern come from a policy. A non-nil limit of 0 explicitly disables
// the guard downstream; max_pattern is validated to 1..64 when set.
func (s *Service) ResolveRepetitionLimit(
	ctx context.Context,
	requested core.RequestedModelSelector,
	resolved core.ModelSelector,
) (limit, maxPattern *int) {
	if s == nil {
		return nil, nil
	}

	snap := s.snapshot()
	userPath := core.UserPathFromContext(ctx)

	var policy VirtualModel
	policyOK := false
	if p, ok := snap.matchingPolicy(resolved.Provider, resolved.Model); ok && userPathAllowed(userPath, p.UserPaths) {
		policy, policyOK = p, true
	}

	if !requested.ExplicitProvider {
		if redirect, ok := snap.findRedirect(requested.Model, userPath, true); ok {
			limit, maxPattern := redirect.vm.RepetitionLimit, redirect.vm.RepetitionMaxPattern
			if limit != nil || maxPattern != nil {
				// Each field independently inherits: nil alias fields are
				// filled from the matching policy before returning.
				if policyOK {
					if limit == nil {
						limit = policy.RepetitionLimit
					}
					if maxPattern == nil {
						maxPattern = policy.RepetitionMaxPattern
					}
				}
				return limit, maxPattern
			}
		}
	}

	if policyOK {
		return policy.RepetitionLimit, policy.RepetitionMaxPattern
	}
	return nil, nil
}
