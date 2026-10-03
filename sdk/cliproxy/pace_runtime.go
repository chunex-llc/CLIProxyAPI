package cliproxy

// paceEnabled reports whether the configured routing strategy is pace.
func (s *Service) paceEnabled() bool {
	return normalizedRoutingRuntimeState(s.cfg).strategy == "pace"
}
