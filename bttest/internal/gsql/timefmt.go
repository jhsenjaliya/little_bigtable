package gsql

import (
	"strconv"
	"strings"
	"time"
)

var (
	weekdayNames = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	monthNames   = []string{"January", "February", "March", "April", "May", "June", "July",
		"August", "September", "October", "November", "December"}
)

func pad(n int64, width int, padChar byte) string {
	s := strconv.FormatInt(n, 10)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	for len(s) < width {
		s = string(padChar) + s
	}
	if neg {
		s = "-" + s
	}
	return s
}

// weekNumber returns the week of the year where weeks start on start and days
// before the first such weekday are in week 0.
func weekNumber(t time.Time, start time.Weekday) int {
	yday := t.YearDay() - 1
	jan1 := time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC).Weekday()
	first := (int(start) - int(jan1) + 7) % 7
	if yday < first {
		return 0
	}
	return (yday-first)/7 + 1
}

func zoneName(t time.Time) string {
	_, off := t.Zone()
	if off == 0 {
		return "UTC"
	}
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	s := "UTC" + sign + strconv.Itoa(off/3600)
	if m := (off % 3600) / 60; m != 0 {
		s += ":" + pad(int64(m), 2, '0')
	}
	return s
}

// formatTime implements FORMAT_TIMESTAMP / FORMAT_DATE format elements.
// micros is the sub-second microsecond component.
func formatTime(f string, t time.Time, micros int64) (string, error) {
	var sb strings.Builder
	for i := 0; i < len(f); i++ {
		c := f[i]
		if c != '%' || i+1 >= len(f) {
			sb.WriteByte(c)
			continue
		}
		i++
		c = f[i]
		// Modifiers.
		if c == 'O' && i+1 < len(f) {
			i++
			c = f[i]
		}
		if c == 'E' && i+1 < len(f) {
			i++
			switch {
			case f[i] == 'z':
				sb.WriteString(formatOffset(t, true))
				continue
			case f[i] == '*' && i+1 < len(f) && f[i+1] == 'S':
				i++
				sb.WriteString(pad(int64(t.Second()), 2, '0'))
				if micros != 0 {
					frac := strings.TrimRight(pad(micros, 6, '0'), "0")
					sb.WriteString("." + frac)
				}
				continue
			case isDigit(f[i]):
				j := i
				for j < len(f) && isDigit(f[j]) {
					j++
				}
				n, _ := strconv.Atoi(f[i:j])
				if j < len(f) && f[j] == 'S' {
					i = j
					sb.WriteString(pad(int64(t.Second()), 2, '0'))
					if n > 0 {
						frac := pad(micros, 6, '0') + "000000000"
						if n > 9 {
							n = 9
						}
						sb.WriteString("." + frac[:n])
					}
					continue
				}
				if j < len(f) && f[j] == 'Y' && n == 4 {
					i = j
					sb.WriteString(pad(int64(t.Year()), 4, '0'))
					continue
				}
				return "", evalErrorf("Invalid format element %%E%s", f[i:min(j+1, len(f))])
			case f[i] == 'c':
				c = 'c'
			case f[i] == 'x':
				c = 'x'
			case f[i] == 'X':
				c = 'X'
			case f[i] == 'Y':
				c = 'Y'
			case f[i] == 'y':
				c = 'y'
			case f[i] == 'C':
				c = 'C'
			default:
				return "", evalErrorf("Invalid format element %%E%c", f[i])
			}
		}
		switch c {
		case 'A':
			sb.WriteString(weekdayNames[t.Weekday()])
		case 'a':
			sb.WriteString(weekdayNames[t.Weekday()][:3])
		case 'B':
			sb.WriteString(monthNames[t.Month()-1])
		case 'b', 'h':
			sb.WriteString(monthNames[t.Month()-1][:3])
		case 'C':
			sb.WriteString(pad(int64(t.Year()/100), 2, '0'))
		case 'c':
			s, _ := formatTime("%a %b %e %H:%M:%S %Y", t, micros)
			sb.WriteString(s)
		case 'D', 'x':
			s, _ := formatTime("%m/%d/%y", t, micros)
			sb.WriteString(s)
		case 'd':
			sb.WriteString(pad(int64(t.Day()), 2, '0'))
		case 'e':
			sb.WriteString(pad(int64(t.Day()), 2, ' '))
		case 'F':
			s, _ := formatTime("%Y-%m-%d", t, micros)
			sb.WriteString(s)
		case 'G':
			y, _ := t.ISOWeek()
			sb.WriteString(pad(int64(y), 4, '0'))
		case 'g':
			y, _ := t.ISOWeek()
			sb.WriteString(pad(int64(y%100), 2, '0'))
		case 'H':
			sb.WriteString(pad(int64(t.Hour()), 2, '0'))
		case 'I':
			h := t.Hour() % 12
			if h == 0 {
				h = 12
			}
			sb.WriteString(pad(int64(h), 2, '0'))
		case 'j':
			sb.WriteString(pad(int64(t.YearDay()), 3, '0'))
		case 'k':
			sb.WriteString(pad(int64(t.Hour()), 2, ' '))
		case 'l':
			h := t.Hour() % 12
			if h == 0 {
				h = 12
			}
			sb.WriteString(pad(int64(h), 2, ' '))
		case 'M':
			sb.WriteString(pad(int64(t.Minute()), 2, '0'))
		case 'm':
			sb.WriteString(pad(int64(t.Month()), 2, '0'))
		case 'n':
			sb.WriteByte('\n')
		case 'P':
			if t.Hour() < 12 {
				sb.WriteString("am")
			} else {
				sb.WriteString("pm")
			}
		case 'p':
			if t.Hour() < 12 {
				sb.WriteString("AM")
			} else {
				sb.WriteString("PM")
			}
		case 'Q':
			sb.WriteString(strconv.Itoa((int(t.Month())-1)/3 + 1))
		case 'R':
			s, _ := formatTime("%H:%M", t, micros)
			sb.WriteString(s)
		case 'r':
			s, _ := formatTime("%I:%M:%S %p", t, micros)
			sb.WriteString(s)
		case 'S':
			sb.WriteString(pad(int64(t.Second()), 2, '0'))
		case 's':
			sb.WriteString(strconv.FormatInt(t.Unix(), 10))
		case 'T', 'X':
			s, _ := formatTime("%H:%M:%S", t, micros)
			sb.WriteString(s)
		case 't':
			sb.WriteByte('\t')
		case 'U':
			sb.WriteString(pad(int64(weekNumber(t, time.Sunday)), 2, '0'))
		case 'u':
			wd := int(t.Weekday())
			if wd == 0 {
				wd = 7
			}
			sb.WriteString(strconv.Itoa(wd))
		case 'V':
			_, w := t.ISOWeek()
			sb.WriteString(pad(int64(w), 2, '0'))
		case 'W':
			sb.WriteString(pad(int64(weekNumber(t, time.Monday)), 2, '0'))
		case 'w':
			sb.WriteString(strconv.Itoa(int(t.Weekday())))
		case 'Y':
			sb.WriteString(pad(int64(t.Year()), 4, '0'))
		case 'y':
			sb.WriteString(pad(int64(t.Year()%100), 2, '0'))
		case 'Z':
			sb.WriteString(zoneName(t))
		case 'z':
			sb.WriteString(strings.ReplaceAll(formatOffset(t, true), ":", ""))
		case '%':
			sb.WriteByte('%')
		default:
			return "", evalErrorf("Invalid format element %%%c", c)
		}
	}
	return sb.String(), nil
}

// parsedTime accumulates fields while parsing.
type parsedTime struct {
	year, month, day          int
	hour, min, sec, micros    int
	pm, hasPM, hour12         bool
	yday                      int
	loc                       *time.Location
	epoch                     int64
	hasEpoch                  bool
	century, yy               int
	hasCentury, hasYY, hasISO bool
}

// parseTimeFormat implements PARSE_TIMESTAMP / PARSE_DATE. Unspecified fields
// default to 1970-01-01 00:00:00 in loc.
func parseTimeFormat(f, s string, loc *time.Location) (time.Time, int64, error) {
	p := &parsedTime{year: 1970, month: 1, day: 1, loc: loc}
	si := 0
	fail := func() (time.Time, int64, error) {
		return time.Time{}, 0, evalErrorf("Failed to parse input string %q with format %q", s, f)
	}
	skipSpace := func() {
		for si < len(s) && (s[si] == ' ' || s[si] == '\t' || s[si] == '\n') {
			si++
		}
	}
	num := func(maxDigits int, allowSign bool) (int, bool) {
		start := si
		if allowSign && si < len(s) && (s[si] == '-' || s[si] == '+') {
			si++
		}
		ds := si
		for si < len(s) && si-ds < maxDigits && isDigit(s[si]) {
			si++
		}
		if si == ds {
			si = start
			return 0, false
		}
		n, err := strconv.Atoi(s[start:si])
		return n, err == nil
	}
	matchName := func(names []string) (int, bool) {
		for i, n := range names {
			for _, cand := range []string{n, n[:3]} {
				if len(s)-si >= len(cand) && strings.EqualFold(s[si:si+len(cand)], cand) {
					si += len(cand)
					return i, true
				}
			}
		}
		return 0, false
	}
	var expand func(f string) bool
	expand = func(f string) bool {
		for i := 0; i < len(f); i++ {
			c := f[i]
			if c == ' ' || c == '\t' || c == '\n' {
				skipSpace()
				continue
			}
			if c != '%' || i+1 >= len(f) {
				if si >= len(s) || s[si] != c {
					return false
				}
				si++
				continue
			}
			i++
			c = f[i]
			if c == 'O' && i+1 < len(f) {
				i++
				c = f[i]
			}
			if c == 'E' && i+1 < len(f) {
				i++
				switch {
				case f[i] == 'z':
					c = 'z'
				case f[i] == '*' && i+1 < len(f) && f[i+1] == 'S':
					i++
					c = 'S'
				case isDigit(f[i]):
					j := i
					for j < len(f) && isDigit(f[j]) {
						j++
					}
					if j >= len(f) {
						return false
					}
					i = j
					c = f[j]
				default:
					c = f[i]
				}
			}
			var ok bool
			var n int
			switch c {
			case 'Y':
				n, ok = num(4, false)
				p.year = n
			case 'y':
				n, ok = num(2, false)
				p.yy, p.hasYY = n, true
			case 'C':
				n, ok = num(2, false)
				p.century, p.hasCentury = n, true
			case 'G':
				n, ok = num(4, false)
				p.year, p.hasISO = n, true
			case 'm':
				n, ok = num(2, false)
				p.month = n
			case 'd', 'e':
				skipSpace()
				n, ok = num(2, false)
				p.day = n
			case 'H', 'k':
				skipSpace()
				n, ok = num(2, false)
				p.hour = n
			case 'I', 'l':
				skipSpace()
				n, ok = num(2, false)
				p.hour, p.hour12 = n, true
			case 'M':
				n, ok = num(2, false)
				p.min = n
			case 'S':
				n, ok = num(2, false)
				p.sec = n
				if ok && si < len(s) && s[si] == '.' {
					j := si + 1
					for j < len(s) && isDigit(s[j]) {
						j++
					}
					frac := (s[si+1:j] + "000000")[:6]
					p.micros, _ = strconv.Atoi(frac)
					si = j
				}
			case 'j':
				n, ok = num(3, false)
				p.yday = n
			case 'p', 'P':
				switch {
				case len(s)-si >= 2 && strings.EqualFold(s[si:si+2], "AM"):
					p.hasPM, p.pm, ok = true, false, true
					si += 2
				case len(s)-si >= 2 && strings.EqualFold(s[si:si+2], "PM"):
					p.hasPM, p.pm, ok = true, true, true
					si += 2
				}
			case 'b', 'B', 'h':
				n, ok = matchName(monthNames)
				p.month = n + 1
			case 'a', 'A':
				_, ok = matchName(weekdayNames)
			case 'u', 'w':
				_, ok = num(1, false)
			case 'U', 'W', 'V':
				_, ok = num(2, false)
			case 'Q':
				n, ok = num(1, false)
				if ok {
					p.month = (n-1)*3 + 1
				}
			case 's':
				var e int
				e, ok = num(19, true)
				p.epoch, p.hasEpoch = int64(e), true
			case 'z':
				j := si
				for j < len(s) && (s[j] == '+' || s[j] == '-' || s[j] == ':' || isDigit(s[j])) {
					j++
				}
				if j > si && (s[si] == '+' || s[si] == '-') {
					l, err := parseOffsetZone(s[si:j])
					if err == nil {
						p.loc, ok = l, true
						si = j
					}
				} else if j < len(s) && (s[si] == 'Z' || s[si] == 'z') {
					p.loc, ok = time.UTC, true
					si++
				}
			case 'Z':
				j := si
				for j < len(s) && s[j] != ' ' {
					j++
				}
				l, err := loadZone(s[si:j])
				if err == nil {
					p.loc, ok = l, true
					si = j
				}
			case 'F':
				ok = expand("%Y-%m-%d")
			case 'T', 'X':
				ok = expand("%H:%M:%S")
			case 'R':
				ok = expand("%H:%M")
			case 'D', 'x':
				ok = expand("%m/%d/%y")
			case 'c':
				ok = expand("%a %b %e %H:%M:%S %Y")
			case 'r':
				ok = expand("%I:%M:%S %p")
			case 'n', 't':
				skipSpace()
				ok = true
			case '%':
				ok = si < len(s) && s[si] == '%'
				si++
			default:
				return false
			}
			if !ok {
				return false
			}
		}
		return true
	}
	if !expand(f) {
		return fail()
	}
	skipSpace()
	if si != len(s) {
		return fail()
	}
	if p.hasEpoch {
		return time.Unix(p.epoch, 0).UTC(), 0, nil
	}
	if p.hasYY {
		if p.hasCentury {
			p.year = p.century*100 + p.yy
		} else if p.yy < 69 {
			p.year = 2000 + p.yy
		} else {
			p.year = 1900 + p.yy
		}
	} else if p.hasCentury {
		p.year = p.century * 100
	}
	if p.hour12 {
		if p.hour < 1 || p.hour > 12 {
			return fail()
		}
		p.hour %= 12
		if p.hasPM && p.pm {
			p.hour += 12
		}
	} else if p.hasPM && p.pm && p.hour < 12 {
		p.hour += 12
	}
	if p.month < 1 || p.month > 12 || p.hour > 23 || p.min > 59 || p.sec > 60 {
		return fail()
	}
	if p.yday > 0 {
		d := time.Date(p.year, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, p.yday-1)
		t := civilToTime(d.Year(), d.Month(), d.Day(), p.hour, p.min, p.sec, p.micros*1000, p.loc)
		return t, int64(p.micros), nil
	}
	if p.day < 1 || p.day > daysIn(time.Month(p.month), p.year) {
		return fail()
	}
	t := civilToTime(p.year, time.Month(p.month), p.day, p.hour, p.min, p.sec, p.micros*1000, p.loc)
	return t, int64(p.micros), nil
}
