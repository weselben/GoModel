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

// ResolvedQualifiedModel returns the concrete qualified model selected for execution.
func (r *RequestModelResolution) ResolvedQualifiedModel() string {
	if r == nil {
		return ""
	}
	return r.ResolvedSelector.QualifiedModel()
}
