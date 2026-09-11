package database

import (
	"database/sql"
	"fmt"
	"time"
)

// TimeLayout is the fixed-width timestamp layout used for every DATETIME
// column in Otter. Fixed width matters: it makes string comparison in SQL
// equivalent to chronological comparison.
const TimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// FormatTime renders a timestamp for storage. The zero time is stored as NULL
// by the helpers below; FormatTime itself always renders a value.
func FormatTime(t time.Time) string {
	return t.UTC().Format(TimeLayout)
}

// ParseTime parses a timestamp previously written by FormatTime.
func ParseTime(s string) (time.Time, error) {
	t, err := time.Parse(TimeLayout, s)
	if err != nil {
		// Be forgiving about values written by hand or by older versions.
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05"} {
			if t, err2 := time.Parse(layout, s); err2 == nil {
				return t.UTC(), nil
			}
		}
		return time.Time{}, fmt.Errorf("database: cannot parse time %q: %w", s, err)
	}
	return t.UTC(), nil
}

// NullableTime is a time.Time that can round-trip through a NULL column.
type NullableTime struct {
	Time  time.Time
	Valid bool
}

// Ptr returns a pointer to the time when valid, otherwise nil.
func (n NullableTime) Ptr() *time.Time {
	if !n.Valid {
		return nil
	}
	t := n.Time
	return &t
}

// Scan implements sql.Scanner.
func (n *NullableTime) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		n.Time, n.Valid = time.Time{}, false
		return nil
	case time.Time:
		n.Time, n.Valid = v.UTC(), true
		return nil
	case string:
		t, err := ParseTime(v)
		if err != nil {
			return err
		}
		n.Time, n.Valid = t, true
		return nil
	case []byte:
		return n.Scan(string(v))
	default:
		return fmt.Errorf("database: cannot scan %T into NullableTime", src)
	}
}

// FormatNullable renders an optional timestamp, using NULL for the zero value.
func FormatNullable(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return FormatTime(*t)
}

// NullableInt renders an optional integer, using NULL for nil.
func NullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// NullableString renders an optional string, using NULL for an empty value.
func NullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// StringOrEmpty dereferences an optional string.
func StringOrEmpty(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}
