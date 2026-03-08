//go:build !linux

package sockopts

func applyFD(_ int, _ Options) error {
	return nil
}
