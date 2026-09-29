// schedule.go — formato propio de schedule del contrato:
//
//	hourly@:15            → cada hora en el minuto 15
//	daily@06:00           → cada día a las 06:00
//	weekly:sun@03:00      → cada domingo a las 03:00
//	monthly:1@02:00       → el día 1 de cada mes a las 02:00
//
// y cálculo de next_run. Horas en horario local del NAS.
package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule — un schedule parseado.
type Schedule struct {
	Kind     string // "hourly" | "daily" | "weekly" | "monthly"
	Minute   int
	Hour     int
	Weekday  time.Weekday // weekly
	MonthDay int          // monthly
	Raw      string
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday,
	"wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday,
	"sat": time.Saturday,
}

// ParseSchedule valida y parsea un schedule del contrato.
func ParseSchedule(raw string) (Schedule, error) {
	s := Schedule{Raw: raw}
	body, hm, found := strings.Cut(raw, "@")
	if !found {
		return s, fmt.Errorf("invalid schedule %q: missing '@'", raw)
	}
	kind, mod, _ := strings.Cut(body, ":")
	s.Kind = kind

	switch kind {
	case "hourly":
		// formato '@:MM' (hm puede venir ':15')
		m := strings.TrimPrefix(hm, ":")
		min, err := strconv.Atoi(m)
		if err != nil || min < 0 || min > 59 || strings.Contains(m, ":") {
			return s, fmt.Errorf("invalid schedule %q: expected an hourly minute ('hourly@:15')", raw)
		}
		s.Minute = min
	case "daily":
		h, m, err := parseHM(hm)
		if err != nil {
			return s, fmt.Errorf("invalid schedule %q: %v", raw, err)
		}
		s.Hour, s.Minute = h, m
	case "weekly":
		wd, ok := weekdays[mod]
		if !ok {
			return s, fmt.Errorf("invalid schedule %q: unknown weekday (sun..sat)", raw)
		}
		h, m, err := parseHM(hm)
		if err != nil {
			return s, fmt.Errorf("invalid schedule %q: %v", raw, err)
		}
		s.Weekday, s.Hour, s.Minute = wd, h, m
	case "monthly":
		d, err := strconv.Atoi(mod)
		if err != nil || d < 1 || d > 28 {
			return s, fmt.Errorf("invalid schedule %q: day of month 1-28", raw)
		}
		h, m, err := parseHM(hm)
		if err != nil {
			return s, fmt.Errorf("invalid schedule %q: %v", raw, err)
		}
		s.MonthDay, s.Hour, s.Minute = d, h, m
	default:
		return s, fmt.Errorf("invalid schedule %q: unknown type (hourly|daily|weekly|monthly)", raw)
	}
	return s, nil
}

// parseHM — 'HH:MM' → (h, m) validados.
func parseHM(s string) (int, int, error) {
	hs, ms, found := strings.Cut(s, ":")
	if !found {
		return 0, 0, fmt.Errorf("expected a time 'HH:MM'")
	}
	h, err1 := strconv.Atoi(hs)
	m, err2 := strconv.Atoi(ms)
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("time out of range")
	}
	return h, m, nil
}

// Next calcula la próxima ejecución estrictamente posterior a 'after'.
// Las horas del schedule se interpretan en horario local del NAS.
func (s Schedule) Next(after time.Time) time.Time {
	t := after.In(time.Local).Truncate(time.Minute).Add(time.Minute) // candidato base
	switch s.Kind {
	case "hourly":
		c := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), s.Minute, 0, 0, t.Location())
		if !c.After(after) {
			c = c.Add(time.Hour)
		}
		return c
	case "daily":
		c := time.Date(t.Year(), t.Month(), t.Day(), s.Hour, s.Minute, 0, 0, t.Location())
		if !c.After(after) {
			c = c.AddDate(0, 0, 1)
		}
		return c
	case "weekly":
		c := time.Date(t.Year(), t.Month(), t.Day(), s.Hour, s.Minute, 0, 0, t.Location())
		for !c.After(after) || c.Weekday() != s.Weekday {
			c = c.AddDate(0, 0, 1)
		}
		return c
	case "monthly":
		c := time.Date(t.Year(), t.Month(), s.MonthDay, s.Hour, s.Minute, 0, 0, t.Location())
		if !c.After(after) {
			c = c.AddDate(0, 1, 0)
		}
		return c
	}
	return t
}

// NextRun — helper exportado para el handler: próxima ejecución de un schedule
// dado su última base ('from' suele ser now o last_run).
func NextRun(raw string, from time.Time) (time.Time, error) {
	s, err := ParseSchedule(raw)
	if err != nil {
		return time.Time{}, err
	}
	return s.Next(from), nil
}

// ParseRetention — '7d' (días), '1m' (30 días), '3m', '1y' (365 días).
func ParseRetention(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty retention")
	}
	unit := s[len(s)-1]
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid retention %q (e.g. 7d, 1m, 3m, 1y)", s)
	}
	switch unit {
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	case 'm':
		return time.Duration(n) * 30 * 24 * time.Hour, nil
	case 'y':
		return time.Duration(n) * 365 * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("invalid retention %q (unit d|m|y)", s)
}
