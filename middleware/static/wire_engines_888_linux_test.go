//go:build linux

package static

import "github.com/goceleris/celeris"

func init() { wireEngines888 = append(wireEngines888, celeris.Epoll) }
