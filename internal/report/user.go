package report

import "os/user"

// lookupUID は uid から名前を引く。引けなければ空。
func lookupUID(uid uint32) string {
	u, err := user.LookupId(itoa(uid))
	if err != nil {
		return ""
	}
	return u.Username
}

func itoa(u uint32) string {
	if u == 0 {
		return "0"
	}
	var b [10]byte
	i := len(b)
	for u > 0 {
		i--
		b[i] = byte('0' + u%10)
		u /= 10
	}
	return string(b[i:])
}
