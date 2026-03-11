package celeris

import (
	"fmt"
	"sync"
)

type nodeType uint8

const (
	static   nodeType = iota
	param             // :id
	catchAll          // *filepath
)

type node struct {
	path     string
	children []*node
	nType    nodeType
	handlers []HandlerFunc
	paramKey string
	fullPath string
}

// router is a compressed radix trie router with a separate tree per HTTP method.
type router struct {
	trees       map[string]*node
	namedRoutes map[string]*Route
	namedMu     sync.RWMutex
}

// Route is an opaque handle to a registered route. Use the Name method to
// assign a name for reverse lookup via [Server.URL].
type Route struct {
	method string
	path   string
	name   string
	router *router
}

// Name sets a name for this route, enabling reverse URL generation via
// [Server.URL]. Panics if a route with the same name is already registered.
func (r *Route) Name(name string) *Route {
	r.name = name
	if r.router != nil {
		r.router.namedMu.Lock()
		if _, exists := r.router.namedRoutes[name]; exists {
			r.router.namedMu.Unlock()
			panic("celeris: duplicate route name: " + name)
		}
		r.router.namedRoutes[name] = r
		r.router.namedMu.Unlock()
	}
	return r
}

// TryName is like [Route.Name] but returns an error instead of panicking when
// a route with the same name already exists.
func (r *Route) TryName(name string) error {
	if r.router == nil {
		r.name = name
		return nil
	}
	r.router.namedMu.Lock()
	defer r.router.namedMu.Unlock()
	if _, exists := r.router.namedRoutes[name]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicateRouteName, name)
	}
	r.name = name
	r.router.namedRoutes[name] = r
	return nil
}

func newRouter() *router {
	return &router{
		trees:       make(map[string]*node),
		namedRoutes: make(map[string]*Route),
	}
}

func (r *router) addRoute(method, path string, handlers []HandlerFunc) *Route {
	if path == "" || path[0] != '/' {
		panic("path must begin with '/'")
	}
	validatePath(path)

	root := r.trees[method]
	if root == nil {
		root = &node{path: "/"}
		r.trees[method] = root
	}

	route := &Route{method: method, path: path, router: r}

	if path == "/" {
		root.handlers = handlers
		root.fullPath = "/"
		return route
	}

	// Split path into segments for insertion.
	segments := splitPath(path)
	current := root
	for _, seg := range segments {
		current = insertChild(current, seg)
	}
	current.handlers = handlers
	current.fullPath = path
	return route
}

// splitPath splits a path like "/users/:id/posts" into
// segments: ["users/", ":id", "posts"].
// The leading "/" is consumed by the root node.
func splitPath(path string) []segment {
	if path == "/" {
		return nil
	}
	// Skip leading /.
	path = path[1:]

	var segs []segment
	for len(path) > 0 {
		switch path[0] {
		case ':':
			end := findSegmentEnd(path)
			segs = append(segs, segment{kind: param, text: path[:end]})
			path = path[end:]
			// Skip following /.
			if len(path) > 0 && path[0] == '/' {
				segs = append(segs, segment{kind: static, text: "/"})
				path = path[1:]
			}
		case '*':
			segs = append(segs, segment{kind: catchAll, text: path})
			return segs
		default:
			// Static text until next special char or end.
			end := len(path)
			for i := range len(path) {
				if path[i] == ':' || path[i] == '*' {
					end = i
					break
				}
			}
			segs = append(segs, segment{kind: static, text: path[:end]})
			path = path[end:]
		}
	}
	return segs
}

type segment struct {
	kind nodeType
	text string
}

func insertChild(parent *node, seg segment) *node {
	switch seg.kind {
	case param:
		key := seg.text[1:] // strip ':'
		for _, ch := range parent.children {
			if ch.nType == param && ch.paramKey == key {
				return ch
			}
		}
		child := &node{nType: param, paramKey: key}
		parent.children = insertSorted(parent.children, child)
		return child

	case catchAll:
		key := seg.text[1:] // strip '*'
		for _, ch := range parent.children {
			if ch.nType == catchAll {
				return ch
			}
		}
		child := &node{nType: catchAll, paramKey: key}
		parent.children = insertSorted(parent.children, child)
		return child

	default: // static
		return insertStatic(parent, seg.text)
	}
}

// insertSorted inserts a node into the children slice maintaining priority
// order: static < param < catchAll. This ensures static routes always take
// precedence over parametric routes regardless of registration order.
func insertSorted(children []*node, child *node) []*node {
	i := 0
	for i < len(children) && children[i].nType <= child.nType {
		i++
	}
	children = append(children, nil)
	copy(children[i+1:], children[i:])
	children[i] = child
	return children
}

func insertStatic(parent *node, text string) *node {
	for _, ch := range parent.children {
		if ch.nType != static {
			continue
		}

		cp := longestPrefix(text, ch.path)
		if cp == 0 {
			continue
		}

		// Full match with existing child.
		if cp == len(ch.path) && cp == len(text) {
			return ch
		}

		// Need to split existing child.
		if cp < len(ch.path) {
			// Split ch into [prefix, suffix].
			split := &node{
				path:     ch.path[cp:],
				children: ch.children,
				handlers: ch.handlers,
				nType:    ch.nType,
				paramKey: ch.paramKey,
				fullPath: ch.fullPath,
			}
			ch.path = ch.path[:cp]
			ch.children = []*node{split}
			ch.handlers = nil
			ch.fullPath = ""
		}

		if cp == len(text) {
			return ch
		}

		// Remaining text to insert.
		return insertStatic(ch, text[cp:])
	}

	// No matching child at all.
	child := &node{path: text}
	parent.children = insertSorted(parent.children, child)
	return child
}

func (r *router) find(method, path string, params *Params) ([]HandlerFunc, string) {
	root := r.trees[method]
	if root == nil {
		return nil, ""
	}

	// Collapse consecutive slashes: "//a///b" → "/a/b".
	path = cleanPath(path)

	if path == "/" {
		return root.handlers, root.fullPath
	}
	if len(path) > 1 && path[0] == '/' {
		path = path[1:]
	}

	return search(root, path, params)
}

func search(n *node, path string, params *Params) ([]HandlerFunc, string) {
	for _, ch := range n.children {
		switch ch.nType {
		case static:
			if len(path) >= len(ch.path) && path[:len(ch.path)] == ch.path {
				remaining := path[len(ch.path):]
				if remaining == "" {
					if ch.handlers != nil {
						return ch.handlers, ch.fullPath
					}
					for _, gc := range ch.children {
						if gc.nType == catchAll {
							*params = append(*params, Param{Key: gc.paramKey, Value: ""})
							return gc.handlers, gc.fullPath
						}
					}
					return nil, ""
				}
				if handlers, fp := search(ch, remaining, params); handlers != nil {
					return handlers, fp
				}
			}

		case param:
			end := findSegmentEnd(path)
			if end == 0 {
				continue
			}
			*params = append(*params, Param{Key: ch.paramKey, Value: path[:end]})
			remaining := path[end:]
			if remaining == "" {
				if ch.handlers != nil {
					return ch.handlers, ch.fullPath
				}
				*params = (*params)[:len(*params)-1]
				continue
			}
			if handlers, fp := search(ch, remaining, params); handlers != nil {
				return handlers, fp
			}
			*params = (*params)[:len(*params)-1]

		case catchAll:
			*params = append(*params, Param{Key: ch.paramKey, Value: "/" + path})
			return ch.handlers, ch.fullPath
		}
	}
	return nil, ""
}

// allowedMethods returns the HTTP methods that have a registered handler for
// the given path, excluding the specified method.
func (r *router) allowedMethods(path string, except string) []string {
	var allowed []string
	var params Params
	for method := range r.trees {
		if method == except {
			continue
		}
		params = params[:0]
		if handlers, _ := r.find(method, path, &params); handlers != nil {
			allowed = append(allowed, method)
		}
	}
	return allowed
}

func (r *router) walk() []RouteInfo {
	var routes []RouteInfo
	for method, root := range r.trees {
		if root.handlers != nil {
			routes = append(routes, RouteInfo{
				Method:       method,
				Path:         root.fullPath,
				HandlerCount: len(root.handlers),
			})
		}
		walkNode(method, root, &routes)
	}
	return routes
}

func walkNode(method string, n *node, routes *[]RouteInfo) {
	for _, ch := range n.children {
		if ch.handlers != nil {
			*routes = append(*routes, RouteInfo{
				Method:       method,
				Path:         ch.fullPath,
				HandlerCount: len(ch.handlers),
			})
		}
		walkNode(method, ch, routes)
	}
}

func longestPrefix(a, b string) int {
	maxLen := len(a)
	if len(b) < maxLen {
		maxLen = len(b)
	}
	for i := range maxLen {
		if a[i] != b[i] {
			return i
		}
	}
	return maxLen
}

func findSegmentEnd(path string) int {
	for i := range len(path) {
		if path[i] == '/' {
			return i
		}
	}
	return len(path)
}

// cleanPath collapses consecutive slashes and ensures a leading slash.
func cleanPath(path string) string {
	if len(path) == 0 {
		return "/"
	}
	hasDouble := false
	for i := 1; i < len(path); i++ {
		if path[i] == '/' && path[i-1] == '/' {
			hasDouble = true
			break
		}
	}
	if !hasDouble {
		return path
	}
	buf := make([]byte, 0, len(path))
	buf = append(buf, path[0])
	for i := 1; i < len(path); i++ {
		if path[i] == '/' && path[i-1] == '/' {
			continue
		}
		buf = append(buf, path[i])
	}
	if len(buf) > 1 && buf[len(buf)-1] == '/' {
		buf = buf[:len(buf)-1]
	}
	return string(buf)
}

func validatePath(path string) {
	for i := 0; i < len(path); i++ {
		switch path[i] {
		case ':':
			end := i + 1
			for end < len(path) && path[end] != '/' {
				end++
			}
			if end == i+1 {
				panic("path contains empty parameter name")
			}
		case '*':
			if i+1 >= len(path) {
				panic("path contains empty catchAll name")
			}
			for j := i + 1; j < len(path); j++ {
				if path[j] == '/' {
					panic("catchAll parameter must be the last path segment")
				}
			}
			return
		}
	}
}
