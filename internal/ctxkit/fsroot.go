package ctxkit

// FilesystemRoot is not implemented yet: the tests of the refusal come first.
func FilesystemRoot(root string) (string, bool) { return "", false }

func isRootOf(vol, rest string, sep byte) bool { return false }
