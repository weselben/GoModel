package core

// RequestModelResolution captures the requested model selector at ingress and
// the concrete selector chosen for execution after alias resolution.
type RequestModelResolution struct {
	Requested        RequestedModelSelector
	ResolvedSelector ModelSelector
	ProviderType     string
	ProviderName     string
	AliasApplied     bool
	// Slowdown is the extra-time factor selected for this request. A value of
	// 0.5 adds 50% of measured inference time; zero disables slowdown.
	Slowdown float64
	// RepetitionLimit is the per-request override of the stream repetition
	// guard's consecutive-repeat abort threshold. Nil inherits the global
	// setting; 0 explicitly disables the guard for this request.
	RepetitionLimit *int
	// RepetitionMaxPattern is the per-request override of the maximum token
	// chain the guard treats as one repeating unit. Nil inherits the global
	// setting; 0 leaves the default (8) in place.
	RepetitionMaxPattern *int
}

// RequestedQualifiedModel returns the canonical requested selector.
func (r *RequestModelResolution) RequestedQualifiedModel() string {
	if r == nil {
		return ""
	}
	return r.Requested.RequestedQualifiedModel()
}

// ResolveRepetitionWithDefaults applies per-request repetition-guard overrides
// on top of service-level defaults. Each override field independently wins
// when set: an explicit limit of 0 disables the guard for the request while
// unset fields inherit the default. A nil resolution is transparent.
func ResolveRepetitionWithDefaults(resolution *RequestModelResolution, defaultLimit, defaultMaxPattern int) (limit, maxPattern int) {
	limit, maxPattern = defaultLimit, defaultMaxPattern
	if resolution == nil {
		return limit, maxPattern
	}
	if v := resolution.RepetitionLimit; v != nil {
		limit = *v
	}
	if v := resolution.RepetitionMaxPattern; v != nil {
		maxPattern = *v
	}
	return limit, maxPattern
}

// ResolvedQualifiedModel returns the concrete qualified model selected for execution.
func (r *RequestModelResolution) ResolvedQualifiedModel() string {
	if r == nil {
		return ""
	}
	return r.ResolvedSelector.QualifiedModel()
}
