package log

import (
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestRecorderSnapshots(t *testing.T) {
	t.Parallel()
	logger, recorder := NewMockLogger()
	values := map[string]any{"names": []string{"original"}}
	group := []slog.Attr{slog.String("key", "original")}
	at := time.Date(2026, 11, 2, 9, 46, 47, 0, time.UTC)
	record := slog.NewRecord(at, slog.LevelWarn, "message", 123)
	record.AddAttrs(slog.Any("data", values), slog.Attr{Key: "group", Value: slog.GroupValue(group...)})
	if err := logger.Handler().Handle(t.Context(), record); err != nil {
		t.Fatal(err)
	}

	values["names"].([]string)[0] = "changed"
	group[0] = slog.String("key", "changed")
	record.AddAttrs(slog.String("later", "not captured"))
	captured := recorder.LastRecord()
	if captured.Time != at || captured.Level != slog.LevelWarn || captured.Message != "message" || captured.PC != 123 {
		t.Fatal("capture changed record metadata")
	}

	want := map[string]any{
		"data":  map[string]any{"names": []string{"original"}},
		"group": map[string]any{"key": "original"},
	}
	if got := RecordFields(captured); !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot fields = %#v, want %#v", got, want)
	}

	RecordFields(captured)["data"].(map[string]any)["names"].([]string)[0] = "changed snapshot"
	if got := RecordFields(recorder.Records()[0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot mutation changed the recorder: %#v", got)
	}

	messages := recorder.Messages()
	messages[0] = "changed snapshot"
	if got := recorder.Messages()[0]; got != "message" {
		t.Fatalf("message snapshot mutation changed the recorder: %q", got)
	}
}

func TestRecorderOwnsCollections(t *testing.T) {
	t.Parallel()
	logger, recorder := NewMockLogger()
	array := [1][]string{{"original"}}
	values := map[string]any{
		"array": array, "nil_slice": []string(nil), "nil_map": map[string]string(nil),
	}
	values["cycle"] = values
	cycle := []any{nil}
	cycle[0] = cycle
	logger.Info("collections", "map", values, "slice", cycle)
	array[0][0] = "changed"
	values["added"] = "not captured"
	cycle[0] = "changed"
	fields := RecordFields(recorder.LastRecord())
	owned := fields["map"].(map[string]any)
	if got := owned["array"].([1][]string)[0][0]; got != "original" {
		t.Fatalf("array's nested slice was not owned: %q", got)
	}
	if _, found := owned["added"]; found {
		t.Fatal("map snapshot retained the source map")
	}
	if owned["nil_slice"].([]string) != nil || owned["nil_map"].(map[string]string) != nil {
		t.Fatal("snapshot changed nil collection values")
	}
	if got := owned["cycle"].(map[string]any)["array"].([1][]string)[0][0]; got != "original" {
		t.Fatal("snapshot did not preserve the map cycle")
	}
	if _, ok := fields["slice"].([]any)[0].([]any); !ok {
		t.Fatal("snapshot did not preserve the slice cycle")
	}
	owned["cycle"].(map[string]any)["array"].([1][]string)[0][0] = "changed snapshot"
	if got := RecordFields(recorder.LastRecord())["map"].(map[string]any)["array"].([1][]string)[0][0]; got != "original" {
		t.Fatal("returned snapshot shares a collection with the recorder")
	}
}

func TestRecorderConcurrentAccess(t *testing.T) {
	t.Parallel()
	logger, recorder := NewMockLogger()
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			logger.Info("concurrent")
			_ = recorder.Records()
			_ = recorder.LastRecord()
			_ = recorder.Messages()
		})
	}

	wg.Wait()
	if got := len(recorder.Records()); got != 50 {
		t.Fatalf("captured %d records, want 50", got)
	}

	recorder.Reset()
	if len(recorder.Messages()) != 0 {
		t.Fatal("Reset retained messages")
	}
}
