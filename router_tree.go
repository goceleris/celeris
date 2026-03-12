package celeris

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

type segment struct {
	kind nodeType
	text string
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
