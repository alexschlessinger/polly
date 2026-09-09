package llm

// IsIterationLimit reports whether every cause of err is ErrMaxIterations.
// A concurrent persistence or provider failure must remain a failure, even
// when the agent also exhausted its iteration allowance.
func IsIterationLimit(err error) bool {
	if err == ErrMaxIterations {
		return true
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		children := multi.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !IsIterationLimit(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return IsIterationLimit(wrapped.Unwrap())
	}
	return false
}
