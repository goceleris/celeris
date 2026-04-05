package celeris

// SkipHelper encapsulates the common middleware skip logic (path map + callback).
// Initialize with Init, then call ShouldSkip at the top of each request.
type SkipHelper struct {
	skipMap map[string]struct{}
	skipFn  func(*Context) bool
}

// Init builds the skip map and stores the skip function.
func (s *SkipHelper) Init(paths []string, fn func(*Context) bool) {
	s.skipFn = fn
	if len(paths) > 0 {
		s.skipMap = make(map[string]struct{}, len(paths))
		for _, p := range paths {
			s.skipMap[p] = struct{}{}
		}
	}
}

// ShouldSkip returns true if the request should bypass the middleware.
func (s *SkipHelper) ShouldSkip(c *Context) bool {
	if s.skipFn != nil && s.skipFn(c) {
		return true
	}
	if len(s.skipMap) > 0 {
		_, ok := s.skipMap[c.Path()]
		return ok
	}
	return false
}
