package probe

import (
	"fmt"
	"strconv"
	"strings"
)

type KernelVersion struct {
	Major int
	Minor int
	Patch int
	Extra string
}

func ParseKernelVersion(s string) (KernelVersion, error) {
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, ' '); idx != -1 {
		s = s[:idx]
	}

	var kv KernelVersion
	parts := strings.SplitN(s, ".", 3)
	if len(parts) < 2 {
		return kv, fmt.Errorf("invalid kernel version: %q", s)
	}

	var err error
	kv.Major, err = strconv.Atoi(parts[0])
	if err != nil {
		return kv, fmt.Errorf("invalid major version: %w", err)
	}
	kv.Minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return kv, fmt.Errorf("invalid minor version: %w", err)
	}

	if len(parts) == 3 {
		patchStr := parts[2]
		for i, c := range patchStr {
			if c < '0' || c > '9' {
				kv.Extra = patchStr[i:]
				patchStr = patchStr[:i]
				break
			}
		}
		if patchStr != "" {
			kv.Patch, err = strconv.Atoi(patchStr)
			if err != nil {
				return kv, fmt.Errorf("invalid patch version: %w", err)
			}
		}
	}

	return kv, nil
}

func (kv KernelVersion) AtLeast(major, minor int) bool {
	if kv.Major != major {
		return kv.Major > major
	}
	return kv.Minor >= minor
}

func (kv KernelVersion) String() string {
	s := fmt.Sprintf("%d.%d.%d", kv.Major, kv.Minor, kv.Patch)
	if kv.Extra != "" {
		s += kv.Extra
	}
	return s
}
