package server

import "testing"

func TestTruncateTopEntities(t *testing.T) {
	t.Parallel()

	items := make([]topEntitySnapshot, topSnapshotCap*100)
	items[0] = topEntitySnapshot{Name: "first.example", Count: 10}
	items[topSnapshotCap-1] = topEntitySnapshot{Name: "last.example", Count: 1}

	got := truncateTopEntities(items)
	if len(got) != topSnapshotCap || cap(got) >= cap(items) {
		t.Fatalf("truncated len/cap = %d/%d, original cap = %d", len(got), cap(got), cap(items))
	}
	if got[0] != items[0] || got[topSnapshotCap-1] != items[topSnapshotCap-1] {
		t.Fatal("truncation changed the retained ranking")
	}
	got[0].Name = "changed.example"
	if items[0].Name != "first.example" {
		t.Fatal("truncated ranking still shares its source array")
	}
}
