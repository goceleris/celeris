package protocol

import (
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"reflect"
)

// OIDTimetz is the type OID of timetz (time with time zone).
const OIDTimetz uint32 = 1266

// -------------------- timetz --------------------

func decodeTimetzBinary(src []byte) (driver.Value, error) {
	if len(src) != 12 {
		return nil, fmt.Errorf("postgres/protocol: timetz length = %d", len(src))
	}
	t := int64(binary.BigEndian.Uint64(src[:8]))
	if t < 0 || t > usecsPerDay {
		return nil, fmt.Errorf("postgres/protocol: timetz out of range: %d microseconds", t)
	}
	// The zone is stored in seconds WEST of UTC; it prints with the opposite
	// sign, as hh, hh:mm or hh:mm:ss (the shortest form that holds it).
	zone := int64(int32(binary.BigEndian.Uint32(src[8:12])))
	if zone <= -16*3600 || zone >= 16*3600 {
		return nil, fmt.Errorf("postgres/protocol: timetz zone out of range: %d seconds", zone)
	}
	buf := appendTimeOfDay(make([]byte, 0, 24), t)
	if zone <= 0 {
		buf = append(buf, '+')
		zone = -zone
	} else {
		buf = append(buf, '-')
	}
	hour, min, sec := zone/3600, zone%3600/60, zone%60
	buf = pad2(buf, hour)
	if sec != 0 {
		buf = append(buf, ':')
		buf = pad2(buf, min)
		buf = append(buf, ':')
		buf = pad2(buf, sec)
	} else if min != 0 {
		buf = append(buf, ':')
		buf = pad2(buf, min)
	}
	return string(buf), nil
}

func init() {
	RegisterType(&TypeCodec{OID: OIDTimetz, Name: "timetz", DecodeBinary: decodeTimetzBinary, ScanType: reflect.TypeOf("")})
}
