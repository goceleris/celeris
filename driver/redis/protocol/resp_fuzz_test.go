package protocol

import "testing"

func FuzzReader(f *testing.F) {
	seeds := []string{
		"+OK\r\n",
		"-ERR\r\n",
		":42\r\n",
		"$3\r\nfoo\r\n",
		"*2\r\n$3\r\nfoo\r\n:1\r\n",
		"_\r\n",
		"#t\r\n",
		",3.14\r\n",
		"%1\r\n+k\r\n+v\r\n",
		"~2\r\n+a\r\n+b\r\n",
		">1\r\n+x\r\n",
		"$-1\r\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewReader()
		r.Feed(data)
		for i := 0; i < 64; i++ {
			v, err := r.Next()
			if err != nil {
				return
			}
			r.Release(v)
		}
	})
}
