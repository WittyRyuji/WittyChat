// Package cache is the desktop local cache (SQLite via modernc driver —
// no cgo). Mirrors conversations, ops, read watermarks and an outgoing
// queue so the client starts instantly offline and syncs deltas.
//
// 桌面端本地缓存：会话/消息/已读水印/待发队列，离线秒开、按水印补差额。
package cache

import (
	"database/sql"
	"encoding/json"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct{ sql *sql.DB }

type Conv struct {
	ID        string
	Kind      string
	Title     string
	LastOpSeq int64
	UpdatedAt time.Time
}

type Op struct {
	ID           string
	ConvID       string
	OpSeq        int64
	Kind         string
	ActorID      string
	ActorName    string
	ClientMsgID  string
	CreatedAt    time.Time
	Payload      json.RawMessage
	PendingLocal bool // outgoing-queue rows before server confirm / 待发
}

type Outbox struct {
	ClientMsgID string
	ConvID      string
	Markdown    string
	ObjectIDs   []string
	CreatedAt   time.Time
}

func Open(path string) (*DB, error) {
	d, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1) // sqlite single writer / 单写者
	db := &DB{sql: d}
	if err := db.migrate(); err != nil {
		return nil, err
	}
	return db, nil
}

func (d *DB) Close() error { return d.sql.Close() }

func (d *DB) migrate() error {
	_, err := d.sql.Exec(`
CREATE TABLE IF NOT EXISTS convs (
  id TEXT PRIMARY KEY, kind TEXT, title TEXT,
  last_op_seq INTEGER, updated_at INTEGER);
CREATE TABLE IF NOT EXISTS ops (
  id TEXT PRIMARY KEY, conv_id TEXT, op_seq INTEGER,
  kind TEXT, actor_id TEXT, actor_name TEXT, client_msg_id TEXT,
  created_at INTEGER, payload BLOB, pending INTEGER DEFAULT 0,
  UNIQUE(conv_id, op_seq));
CREATE INDEX IF NOT EXISTS ops_conv ON ops(conv_id, op_seq);
CREATE TABLE IF NOT EXISTS watermarks (
  conv_id TEXT, user_id TEXT, last_read_seq INTEGER,
  PRIMARY KEY (conv_id, user_id));
CREATE TABLE IF NOT EXISTS outbox (
  client_msg_id TEXT PRIMARY KEY, conv_id TEXT,
  markdown TEXT, object_ids TEXT, created_at INTEGER);
CREATE TABLE IF NOT EXISTS kv (k TEXT PRIMARY KEY, v TEXT);`)
	return err
}

func (d *DB) UpsertConv(c Conv) error {
	_, err := d.sql.Exec(
		`INSERT INTO convs (id,kind,title,last_op_seq,updated_at) VALUES (?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET title=excluded.title,
		   last_op_seq=max(convs.last_op_seq, excluded.last_op_seq),
		   updated_at=excluded.updated_at`,
		c.ID, c.Kind, c.Title, c.LastOpSeq, c.UpdatedAt.UnixMilli())
	return err
}

func (d *DB) Convs() ([]Conv, error) {
	rows, err := d.sql.Query(`SELECT id,kind,title,last_op_seq,updated_at FROM convs ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Conv
	for rows.Next() {
		var c Conv
		var ms int64
		if err := rows.Scan(&c.ID, &c.Kind, &c.Title, &c.LastOpSeq, &ms); err != nil {
			return nil, err
		}
		c.UpdatedAt = time.UnixMilli(ms)
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpsertOp stores one op (or marks it pending for the outbox mirror).
func (d *DB) UpsertOp(o Op) error {
	_, err := d.sql.Exec(
		`INSERT INTO ops (id,conv_id,op_seq,kind,actor_id,actor_name,client_msg_id,created_at,payload,pending)
		 VALUES (?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET op_seq=excluded.op_seq, payload=excluded.payload, pending=0`,
		o.ID, o.ConvID, o.OpSeq, o.Kind, o.ActorID, o.ActorName,
		o.ClientMsgID, o.CreatedAt.UnixMilli(), o.Payload, bool2int(o.PendingLocal))
	return err
}

func (d *DB) Ops(convID string, limit int) ([]Op, error) {
	rows, err := d.sql.Query(
		`SELECT id,conv_id,op_seq,kind,actor_id,actor_name,client_msg_id,created_at,payload,pending
		 FROM ops WHERE conv_id=? ORDER BY op_seq DESC LIMIT ?`, convID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Op
	for rows.Next() {
		var o Op
		var ms, pend int64
		if err := rows.Scan(&o.ID, &o.ConvID, &o.OpSeq, &o.Kind, &o.ActorID,
			&o.ActorName, &o.ClientMsgID, &ms, &o.Payload, &pend); err != nil {
			return nil, err
		}
		o.CreatedAt = time.UnixMilli(ms)
		o.PendingLocal = pend != 0
		out = append(out, o)
	}
	return out, rows.Err()
}

func (d *DB) SetWatermark(convID, userID string, seq int64) error {
	_, err := d.sql.Exec(
		`INSERT INTO watermarks (conv_id,user_id,last_read_seq) VALUES (?,?,?)
		 ON CONFLICT(conv_id,user_id) DO UPDATE SET last_read_seq=max(last_read_seq, excluded.last_read_seq)`,
		convID, userID, seq)
	return err
}

func (d *DB) Watermarks(convID string) (map[string]int64, error) {
	rows, err := d.sql.Query(`SELECT user_id,last_read_seq FROM watermarks WHERE conv_id=?`, convID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	m := map[string]int64{}
	for rows.Next() {
		var u string
		var s int64
		if err := rows.Scan(&u, &s); err != nil {
			return nil, err
		}
		m[u] = s
	}
	return m, rows.Err()
}

// Outgoing queue: pending sends survive restarts.
func (d *DB) Enqueue(o Outbox) error {
	ids, _ := json.Marshal(o.ObjectIDs)
	_, err := d.sql.Exec(
		`INSERT INTO outbox (client_msg_id,conv_id,markdown,object_ids,created_at) VALUES (?,?,?,?,?)
		 ON CONFLICT(client_msg_id) DO NOTHING`,
		o.ClientMsgID, o.ConvID, o.Markdown, string(ids), o.CreatedAt.UnixMilli())
	return err
}

func (d *DB) PendingOutbox() ([]Outbox, error) {
	rows, err := d.sql.Query(`SELECT client_msg_id,conv_id,markdown,object_ids,created_at FROM outbox ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Outbox
	for rows.Next() {
		var o Outbox
		var ids string
		var ms int64
		if err := rows.Scan(&o.ClientMsgID, &o.ConvID, &o.Markdown, &ids, &ms); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(ids), &o.ObjectIDs)
		o.CreatedAt = time.UnixMilli(ms)
		out = append(out, o)
	}
	return out, rows.Err()
}

func (d *DB) Dequeue(clientMsgID string) error {
	_, err := d.sql.Exec(`DELETE FROM outbox WHERE client_msg_id=?`, clientMsgID)
	return err
}

func (d *DB) KVGet(k string) (string, error) {
	var v string
	err := d.sql.QueryRow(`SELECT v FROM kv WHERE k=?`, k).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (d *DB) KVSet(k, v string) error {
	_, err := d.sql.Exec(`INSERT INTO kv (k,v) VALUES (?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

func bool2int(b bool) int {
	if b {
		return 1
	}
	return 0
}
