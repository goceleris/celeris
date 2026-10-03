// Package sub uses the root package.
package sub

import "example.com/fix"

func Use(k fix.Kind) fix.Box[string] { return fix.Box[string]{} }
