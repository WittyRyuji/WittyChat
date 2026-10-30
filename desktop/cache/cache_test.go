package cache

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheRoundtrip(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	c := Conv{ID: "c1", Kind: "dm", Title: "Alice", LastOpSeq: 3, UpdatedAt: time.Now()}
	if err := db.UpsertConv(c); err != nil {
		t.Fatal(err)
	}
	convs, err := db.Convs()
	if err != nil || len(convs) != 1 || convs[0].Title != "Alice" {
		t.Fatalf("convs: %v %v", convs, err)
	}

	op := Op{
		ID: "o1", ConvID: "c1", OpSeq: 3, Kind: "message", ActorID: "u1",
		ActorName: "Alice", ClientMsgID: "m1", CreatedAt: time.Now(),
		Payload: json.RawMessage(`{"markdown":"hi"}`),
	}
	if err := db.UpsertOp(op); err != nil {
		t.Fatal(err)
	}
	ops, err := db.Ops("c1", 10)
	if err != nil || len(ops) != 1 || ops[0].OpSeq != 3 {
		t.Fatalf("ops: %v %v", ops, err)
	}

	if err := db.SetWatermark("c1", "u1", 3); err != nil {
		t.Fatal(err)
	}
	if err := db.SetWatermark("c1", "u1", 2); err != nil { // older seq must not regress
		t.Fatal(err)
	}
	wm, _ := db.Watermarks("c1")
	if wm["u1"] != 3 {
		t.Fatalf("watermark regressed: %v", wm)
	}

	if err := db.Enqueue(Outbox{
		ClientMsgID: "m9", ConvID: "c1", Markdown: "pending",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	pend, _ := db.PendingOutbox()
	if len(pend) != 1 || pend[0].ClientMsgID != "m9" {
		t.Fatalf("outbox: %v", pend)
	}
	if err := db.Dequeue("m9"); err != nil {
		t.Fatal(err)
	}
	pend, _ = db.PendingOutbox()
	if len(pend) != 0 {
		t.Fatalf("outbox not drained: %v", pend)
	}

	if err := db.KVSet("me", "u1"); err != nil {
		t.Fatal(err)
	}
	v, _ := db.KVGet("me")
	if v != "u1" {
		t.Fatalf("kv: %q", v)
	}
}
