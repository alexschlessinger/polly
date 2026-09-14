package llm

// truth returns a pointer to v for capability fixtures, where nil means unknown.
func truth(v bool) *bool { return &v }
