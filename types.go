package monty

import (
	"fmt"
	"math"
	"time"
)

// Valuer is implemented by types that define their own Monty value form.
// Anywhere the package converts Go values — From, run inputs, Call.Return,
// host function results — a Valuer is honored before reflection rules apply.
//
// The payload types in this package (Date, DateTime, TimeDelta, TimeZone,
// Path, NamedTuple, DataclassValue, Exception, StatResult) all implement
// Valuer, so their struct literals double as constructors:
//
//	monty.Date{Year: 2026, Month: time.May, Day: 12}
type Valuer interface {
	MontyValue() Value
}

// Date is a Python datetime.date.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// MontyValue implements Valuer.
func (d Date) MontyValue() Value {
	return Value{kind: DateKind, valueExtra: &valueExtra{date: d}}
}

// Time returns the date as midnight UTC.
func (d Date) Time() time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
}

// DateOf converts a Go time to its calendar date.
func DateOf(t time.Time) Date {
	return Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}
}

// TimeZone is a Python datetime.timezone: a fixed offset from UTC with an
// optional name. Offsets are truncated to whole seconds when crossing into
// Python.
type TimeZone struct {
	Offset time.Duration
	Name   string
}

// MontyValue implements Valuer.
func (z TimeZone) MontyValue() Value {
	return Value{kind: TimeZoneKind, valueExtra: &valueExtra{timezone: z}}
}

// Location returns the timezone as a fixed *time.Location.
func (z TimeZone) Location() *time.Location {
	return time.FixedZone(z.Name, int(z.Offset/time.Second))
}

// DateTime is a Python datetime.datetime. A nil TZ is a naive datetime;
// a non-nil TZ carries the UTC offset (and optional name) of an aware one.
type DateTime struct {
	Year        int
	Month       time.Month
	Day         int
	Hour        int
	Minute      int
	Second      int
	Microsecond int
	TZ          *TimeZone
}

// MontyValue implements Valuer.
func (dt DateTime) MontyValue() Value {
	return Value{kind: DateTimeKind, valueExtra: &valueExtra{datetime: dt}}
}

// Time converts the datetime to a Go time.Time. An aware datetime is placed
// in a fixed zone built from its offset and name; a naive datetime is
// interpreted as UTC (inspect TZ to distinguish the two).
func (dt DateTime) Time() time.Time {
	location := time.UTC
	if dt.TZ != nil {
		location = dt.TZ.Location()
	}
	return time.Date(
		dt.Year,
		dt.Month,
		dt.Day,
		dt.Hour,
		dt.Minute,
		dt.Second,
		dt.Microsecond*int(time.Microsecond),
		location,
	)
}

// DateTimeOf converts a Go time to an aware Python datetime payload.
func DateTimeOf(t time.Time) DateTime {
	name, offset := t.Zone()
	return DateTime{
		Year:        t.Year(),
		Month:       t.Month(),
		Day:         t.Day(),
		Hour:        t.Hour(),
		Minute:      t.Minute(),
		Second:      t.Second(),
		Microsecond: t.Nanosecond() / int(time.Microsecond),
		TZ:          &TimeZone{Offset: time.Duration(offset) * time.Second, Name: name},
	}
}

// TimeDelta is a Python datetime.timedelta. Python's range exceeds
// time.Duration, so the fields mirror Python's normalized representation.
type TimeDelta struct {
	Days         int
	Seconds      int
	Microseconds int
}

// MontyValue implements Valuer.
func (d TimeDelta) MontyValue() Value {
	return Value{kind: TimeDeltaKind, valueExtra: &valueExtra{timedelta: d}}
}

// Duration converts the timedelta to a time.Duration, reporting false when it
// does not fit.
func (d TimeDelta) Duration() (time.Duration, bool) {
	const (
		microsPerSecond = int64(1_000_000)
		microsPerDay    = int64(86_400_000_000)
	)
	maxMicros := int64(math.MaxInt64) / int64(time.Microsecond)
	minMicros := int64(math.MinInt64) / int64(time.Microsecond)
	days := int64(d.Days)
	if days > maxMicros/microsPerDay || days < minMicros/microsPerDay {
		return 0, false
	}
	totalMicros := days*microsPerDay + int64(d.Seconds)*microsPerSecond + int64(d.Microseconds)
	if totalMicros > maxMicros || totalMicros < minMicros {
		return 0, false
	}
	return time.Duration(totalMicros) * time.Microsecond, true
}

// TimeDeltaOf converts a Go duration to a normalized Python timedelta.
func TimeDeltaOf(duration time.Duration) TimeDelta {
	totalMicros := int64(duration / time.Microsecond)
	days := totalMicros / 86_400_000_000
	remaining := totalMicros % 86_400_000_000
	if remaining < 0 {
		days--
		remaining += 86_400_000_000
	}
	seconds := remaining / 1_000_000
	micros := remaining % 1_000_000
	return TimeDelta{
		Days:         int(days),
		Seconds:      int(seconds),
		Microseconds: int(micros),
	}
}

// Path is a Python pathlib.Path.
type Path string

// MontyValue implements Valuer.
func (p Path) MontyValue() Value {
	return Value{kind: PathKind, text: string(p)}
}

// NamedTuple is a Python namedtuple: a tuple whose elements carry field names.
type NamedTuple struct {
	Type   string
	Fields []string
	Values []Value
}

// MontyValue implements Valuer.
func (nt NamedTuple) MontyValue() Value {
	return Value{
		kind:  NamedTupleKind,
		items: nt.Values,
		valueExtra: &valueExtra{
			typeName:   nt.Type,
			fieldNames: nt.Fields,
		},
	}
}

// DataclassValue is a Python dataclass instance: an ordered set of attributes
// with a class name and identity. Register Go struct equivalents with
// DataclassFor and WithDataclasses for round-tripping.
type DataclassValue struct {
	Name   string
	TypeID uint64
	Fields []string
	Attrs  []Pair
	Frozen bool
}

// MontyValue implements Valuer.
func (d DataclassValue) MontyValue() Value {
	return Value{
		kind:  DataclassKind,
		pairs: d.Attrs,
		valueExtra: &valueExtra{
			typeName:   d.Name,
			typeID:     d.TypeID,
			fieldNames: d.Fields,
			frozen:     d.Frozen,
		},
	}
}

// Exception is a Python exception, usable both as a value (Valuer) and as a
// Go error. Returning a *Exception from a host function or Call.Raise raises
// that exception type inside Python.
type Exception struct {
	// Type is the Python exception type name, such as "ValueError".
	Type string
	// Message is the exception message.
	Message string
}

// MontyValue implements Valuer.
func (e Exception) MontyValue() Value {
	excType := e.Type
	if excType == "" {
		excType = "RuntimeError"
	}
	return Value{kind: ExceptionKind, text: e.Message, valueExtra: &valueExtra{typeName: excType}}
}

// Error implements the error interface as "Type: Message".
func (e *Exception) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return e.Type
	}
	return e.Type + ": " + e.Message
}

// Errorf builds a typed Python exception error. The type must be one of the
// exception names Monty understands ("ValueError", "KeyError", ...).
func Errorf(excType, format string, args ...any) *Exception {
	return &Exception{Type: excType, Message: fmt.Sprintf(format, args...)}
}
