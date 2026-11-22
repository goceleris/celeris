package postgres

import "testing"

func TestStartupParamsPins(t *testing.T) {
	for _, c := range []struct {
		name string
		in   map[string]string
		want map[string]string
	}{
		{"nil", nil, map[string]string{"extra_float_digits": "3", "DateStyle": "ISO", "bytea_output": "hex"}},
		{"unrelated", map[string]string{"search_path": "x"}, map[string]string{"search_path": "x", "extra_float_digits": "3", "DateStyle": "ISO", "bytea_output": "hex"}},
		{"explicit float digits", map[string]string{"extra_float_digits": "0"}, map[string]string{"extra_float_digits": "0", "DateStyle": "ISO", "bytea_output": "hex"}},
		{"explicit datestyle any case", map[string]string{"datestyle": "SQL"}, map[string]string{"datestyle": "SQL", "extra_float_digits": "3", "bytea_output": "hex"}},
		{"all three", map[string]string{"extra_float_digits": "1", "DateStyle": "German", "bytea_output": "escape"}, map[string]string{"extra_float_digits": "1", "DateStyle": "German", "bytea_output": "escape"}},
		{"in options", map[string]string{"options": "-c DateStyle=SQL -c extra_float_digits=0 -c bytea_output=escape"}, map[string]string{"options": "-c DateStyle=SQL -c extra_float_digits=0 -c bytea_output=escape"}},
	} {
		in := map[string]string{}
		for k, v := range c.in {
			in[k] = v
		}
		got := startupParams(c.in)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s: got %v, want %v", c.name, got, c.want)
				break
			}
		}
		if len(c.in) != len(in) {
			t.Errorf("%s: input map modified: %v", c.name, c.in)
		}
	}
}
