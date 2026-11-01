package codex

import (
	"bytes"
	"regexp"
	"strings"
	"time"
)

// exhaustedUntil is when every exhausted window has reset, or 0 if none is.
func (s rateLimitSnapshot) exhaustedUntil() int64 {
	var until int64
	for _, w := range []*rateLimitWindow{s.Primary, s.Secondary} {
		if w != nil && w.UsedPercent >= 100 && w.ResetsAt != nil && *w.ResetsAt > until {
			until = *w.ResetsAt
		}
	}
	return until
}

func isUsageLimit(info []byte) bool {
	return bytes.Equal(bytes.TrimSpace(info), []byte(`"usageLimitExceeded"`))
}

// limitResetLocked prefers the rate-limit snapshot app-server sends just
// before the error; the error text only has minute precision.
func (c *Client) limitResetLocked(message string, now time.Time) int64 {
	var until int64
	for _, u := range c.exhausted {
		if u > now.Unix() {
			until = max(until, u)
		}
	}
	if until > 0 {
		return until
	}
	if t, ok := parseRetryAt(message, now); ok {
		return t.Add(time.Minute).Unix()
	}
	return 0
}

var ordinalDay = regexp.MustCompile(`(\d)(st|nd|rd|th),`)

// parseRetryAt reads the "Try again at 3:04 PM." suffix codex appends to a
// usage-limit message, in this machine's local time: "3:04 PM" for today,
// "Oct 25th, 2026 3:04 PM" otherwise.
func parseRetryAt(message string, now time.Time) (time.Time, bool) {
	const marker = "try again at "
	i := strings.LastIndex(strings.ToLower(message), marker)
	if i < 0 {
		return time.Time{}, false
	}
	s := strings.TrimSpace(message[i+len(marker):])
	s = strings.TrimSuffix(s, ".")
	loc := now.Location()
	if t, err := time.ParseInLocation("3:04 PM", s, loc); err == nil {
		y, m, d := now.Date()
		return time.Date(y, m, d, t.Hour(), t.Minute(), 0, 0, loc), true
	}
	if t, err := time.ParseInLocation("Jan 2, 2006 3:04 PM", ordinalDay.ReplaceAllString(s, "$1,"), loc); err == nil {
		return t, true
	}
	return time.Time{}, false
}
