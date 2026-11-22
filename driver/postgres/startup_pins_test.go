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
		got := startupParams(c.in, Options{})
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

// A pin a server refused (dropPins) and startup_pins=off are not sent; the
// DSN's own parameters are.
func TestStartupParamsDroppedAndOff(t *testing.T) {
	dsn := map[string]string{"search_path": "x"}
	for _, c := range []struct {
		name string
		o    Options
		want map[string]string
	}{
		{"drop extra_float_digits", Options{dropPins: 1 << 0}, map[string]string{"search_path": "x", "DateStyle": "ISO", "bytea_output": "hex"}},
		{"drop DateStyle", Options{dropPins: 1 << 1}, map[string]string{"search_path": "x", "extra_float_digits": "3", "bytea_output": "hex"}},
		{"drop bytea_output", Options{dropPins: 1 << 2}, map[string]string{"search_path": "x", "extra_float_digits": "3", "DateStyle": "ISO"}},
		{"drop all", Options{dropPins: 1<<0 | 1<<1 | 1<<2}, map[string]string{"search_path": "x"}},
		{"off", Options{startupPinsOff: true}, map[string]string{"search_path": "x"}},
	} {
		got := startupParams(dsn, c.o)
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
	}
}

func TestParseDSNStartupPins(t *testing.T) {
	for raw, off := range map[string]bool{
		"postgres://u@h/d?startup_pins=off":   true,
		"postgres://u@h/d?startup_pins=false": true,
		"host=h user=u startup_pins=0":        true,
		"postgres://u@h/d?startup_pins=on":    false,
		"postgres://u@h/d":                    false,
	} {
		d, err := ParseDSN(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if d.Options.startupPinsOff != off {
			t.Errorf("%s: startupPinsOff = %v, want %v", raw, d.Options.startupPinsOff, off)
		}
		if _, ok := d.Params["startup_pins"]; ok {
			t.Errorf("%s: startup_pins is a driver key, it must not be sent to the server", raw)
		}
	}
	if _, err := ParseDSN("postgres://u@h/d?startup_pins=maybe"); err == nil {
		t.Error("startup_pins=maybe: want an error")
	}
}
