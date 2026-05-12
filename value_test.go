package monty

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestValueDefensiveCopies(t *testing.T) {
	sourceBytes := []byte("abc")
	bytesValue := Bytes(sourceBytes)
	sourceBytes[0] = 'z'
	if got := string(bytesValue.Bytes()); got != "abc" {
		t.Fatalf("bytes = %q, want abc", got)
	}
	copiedBytes := bytesValue.Bytes()
	copiedBytes[1] = 'z'
	if got := string(bytesValue.Bytes()); got != "abc" {
		t.Fatalf("bytes after mutation = %q, want abc", got)
	}

	list := List(Int(1), Int(2))
	items := list.Items()
	items[0] = Int(99)
	if got := list.Items()[0].Int(); got != 1 {
		t.Fatalf("list item = %d, want 1", got)
	}

	dict := Dict(Pair{Key: Str("x"), Value: Int(1)})
	pairs := dict.Pairs()
	pairs[0].Value = Int(99)
	if got := dict.Pairs()[0].Value.Int(); got != 1 {
		t.Fatalf("dict value = %d, want 1", got)
	}
}

func TestFromStruct(t *testing.T) {
	type sample struct {
		Count int    `monty:"count"`
		Name  string `monty:"name"`
	}

	value, err := From(sample{Count: 2, Name: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if value.Kind() != DictKind {
		t.Fatalf("kind = %s, want Dict", value.Kind())
	}
	if got := value.Pairs()[0].Key.Str(); got != "count" {
		t.Fatalf("first key = %q, want count", got)
	}
}

func TestStringDict(t *testing.T) {
	dict := StringDict(map[string]Value{"x": Int(1)})
	if dict.Kind() != DictKind {
		t.Fatalf("kind = %s, want Dict", dict.Kind())
	}
	if got := dict.Pairs()[0].Key.Str(); got != "x" {
		t.Fatalf("key = %q, want x", got)
	}
}

func TestRichDateTimeValues(t *testing.T) {
	program, err := Compile(`
from datetime import date, datetime, timedelta, timezone
(
    date(2024, 5, 6),
    datetime(2024, 5, 6, 7, 8, 9, 123456, tzinfo=timezone(timedelta(hours=-5), "CDT")),
    timedelta(days=2, seconds=3, microseconds=4),
    timezone(timedelta(hours=5, minutes=30), "IST"),
)
`)
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	value, err := program.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	items := value.Items()
	if got := items[0].Kind(); got != DateKind {
		t.Fatalf("date kind = %s, want Date", got)
	}
	if got, want := items[0].Date(), (MontyDate{Year: 2024, Month: time.May, Day: 6}); got != want {
		t.Fatalf("date = %+v, want %+v", got, want)
	}
	datetime := items[1].DateTime()
	if items[1].Kind() != DateTimeKind || datetime.Year != 2024 || datetime.Microsecond != 123456 || !datetime.HasOffset || datetime.OffsetSeconds != -5*60*60 {
		t.Fatalf("datetime = kind %s payload %+v", items[1].Kind(), datetime)
	}
	delta := items[2].TimeDelta()
	if items[2].Kind() != TimeDeltaKind || delta != (MontyTimeDelta{Days: 2, Seconds: 3, Microseconds: 4}) {
		t.Fatalf("timedelta = kind %s payload %+v", items[2].Kind(), delta)
	}
	zone := items[3].TimeZone()
	if items[3].Kind() != TimeZoneKind || !zone.HasName || zone.Name != "IST" || zone.OffsetSeconds != 5*60*60+30*60 {
		t.Fatalf("timezone = kind %s payload %+v", items[3].Kind(), zone)
	}
}

func TestRichNamedTupleAndDataclassValues(t *testing.T) {
	program, err := Compile(`point`, WithInputs("point"))
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	value, err := program.Run(context.Background(), Inputs{
		"point": NamedTuple("Point", []string{"x", "y"}, Int(1), Int(2)),
	})
	if err != nil {
		t.Fatal(err)
	}
	named := value.NamedTuple()
	if value.Kind() != NamedTupleKind || named.TypeName != "Point" || !reflect.DeepEqual(named.FieldNames, []string{"x", "y"}) {
		t.Fatalf("namedtuple = kind %s payload %+v", value.Kind(), named)
	}
	if got := named.Values[1].Int(); got != 2 {
		t.Fatalf("namedtuple y = %d, want 2", got)
	}

	dataclassProgram, err := Compile(`user`, WithInputs("user"))
	if err != nil {
		t.Fatal(err)
	}
	defer dataclassProgram.Close()
	dataclassValue, err := dataclassProgram.Run(context.Background(), Inputs{
		"user": Dataclass("User", 42, []string{"id", "name"}, []Pair{
			{Key: Str("id"), Value: Int(7)},
			{Key: Str("name"), Value: Str("Ada")},
		}, true),
	})
	if err != nil {
		t.Fatal(err)
	}
	dataclass := dataclassValue.Dataclass()
	if dataclassValue.Kind() != DataclassKind || dataclass.Name != "User" || dataclass.TypeID != 42 || !dataclass.Frozen || !reflect.DeepEqual(dataclass.FieldNames, []string{"id", "name"}) {
		t.Fatalf("dataclass = kind %s payload %+v", dataclassValue.Kind(), dataclass)
	}
	if got := dataclass.Attrs[1].Value.Str(); got != "Ada" {
		t.Fatalf("dataclass name = %q, want Ada", got)
	}
}

func TestRichValuesAsInputs(t *testing.T) {
	program, err := Compile(`
(
    point.x + point.y,
    event.year,
    delta.total_seconds(),
    zone,
    tags == {1, 2},
)
`, WithInputs("point", "event", "delta", "zone", "tags"))
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	result, err := program.Run(context.Background(), Inputs{
		"point": NamedTuple("Point", []string{"x", "y"}, Int(3), Int(4)),
		"event": DateTime(MontyDateTime{
			Year:         2026,
			Month:        time.May,
			Day:          12,
			Hour:         9,
			HasOffset:    true,
			TimezoneName: "UTC",
		}),
		"delta": TimeDeltaValue(MontyTimeDeltaFromDuration(90 * time.Second)),
		"zone":  TimeZone(2*60*60, "EET"),
		"tags":  Set(Int(1), Int(2)),
	})
	if err != nil {
		t.Fatal(err)
	}
	items := result.Items()
	if got := []int{items[0].Int(), items[1].Int(), int(items[2].Float64())}; !reflect.DeepEqual(got, []int{7, 2026, 90}) {
		t.Fatalf("numeric results = %v", got)
	}
	if zone := items[3].TimeZone(); items[3].Kind() != TimeZoneKind || zone.OffsetSeconds != 7200 || zone.Name != "EET" {
		t.Fatalf("timezone result = kind %s payload %+v", items[3].Kind(), zone)
	}
	if !items[4].Bool() {
		t.Fatal("set input did not compare equal")
	}
}

func TestNestedEmptyRawInputs(t *testing.T) {
	program, err := Compile(`(nested[0], nested[1])`, WithInputs("nested"))
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()

	result, err := program.Run(context.Background(), Inputs{
		"nested": List(List(), Dict()),
	})
	if err != nil {
		t.Fatal(err)
	}
	items := result.Items()
	if got := len(items[0].Items()); items[0].Kind() != ListKind || got != 0 {
		t.Fatalf("nested list = kind %s len %d, want empty list", items[0].Kind(), got)
	}
	if got := len(items[1].Pairs()); items[1].Kind() != DictKind || got != 0 {
		t.Fatalf("nested dict = kind %s len %d, want empty dict", items[1].Kind(), got)
	}
}

func TestAsSlicesAndMaps(t *testing.T) {
	ints, err := As[[]int](List(Int(1), Int(2), Int(3)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ints, []int{1, 2, 3}) {
		t.Fatalf("slice = %v", ints)
	}
	values, err := As[map[string]int](StringDict(map[string]Value{"x": Int(1), "y": Int(2)}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(values, map[string]int{"x": 1, "y": 2}) {
		t.Fatalf("map = %v", values)
	}
}
