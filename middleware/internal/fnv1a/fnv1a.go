// Package fnv1a provides a fast, allocation-free FNV-1a string hash and
// power-of-two rounding for sharded data structures.
package fnv1a

// Hash returns the FNV-1a hash of s.
func Hash(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// NextPow2 returns the smallest power of 2 >= n, minimum 1.
func NextPow2(n int) int {
	if n <= 1 {
		return 1
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n |= n >> 32
	return n + 1
}
