package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestReadStatementsReuseFreshBindingsAndSchema(t *testing.T) {
	s := testStore(t)
	if _, err := s.db.Exec(`CREATE TABLE read_plan_probe(id INTEGER PRIMARY KEY,value TEXT); INSERT INTO read_plan_probe VALUES(1,'first'),(2,'second')`); err != nil {
		t.Fatal(err)
	}
	query := `SELECT value FROM read_plan_probe WHERE id=?`
	var value string
	if err := s.readRowContext(t.Context(), query, 1).Scan(&value); err != nil || value != "first" {
		t.Fatal(value, err)
	}
	first := s.db.readPlans.items[query]
	if first == nil {
		t.Fatal("actual read route did not retain a statement")
	}
	if err := s.readRowContext(t.Context(), query, 2).Scan(&value); err != nil || value != "second" || s.db.readPlans.items[query] != first {
		t.Fatal("statement or argument binding was not reused correctly", value, err)
	}
	if err := s.policyRowContext(t.Context(), query, 1).Scan(&value); err != nil || value != "first" || s.db.policyPlans.items[query] == nil || s.db.policyPlans.items[query] == first {
		t.Fatal("reader and policy plans were not isolated", err)
	}
	if _, err := s.db.Exec(`ALTER TABLE read_plan_probe ADD COLUMN extra TEXT; UPDATE read_plan_probe SET value='changed' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := s.readRowContext(t.Context(), query, 1).Scan(&value); err != nil || value != "changed" || s.db.readPlans.items[query] != first {
		t.Fatal("plan reused stale rows or failed schema reprepare", value, err)
	}
	if _, err := s.db.Exec(`DROP TABLE read_plan_probe`); err != nil {
		t.Fatal(err)
	}
	if err := s.readRowContext(t.Context(), query, 1).Scan(&value); err == nil {
		t.Fatal("cached statement concealed schema failure")
	}
	if s.readDB().Stats().InUse != 0 || s.policyDB().Stats().InUse != 0 {
		t.Fatal("read left a connection or transaction held")
	}
}

func TestReadStatementsPolicyStillReadsFreshRevocationAndFaults(t *testing.T) {
	s, _ := featureFixture(t)
	client := admissionClient("prepared-policy-synthetic")
	if err := s.RecordSession(client); err != nil {
		t.Fatal(err)
	}
	if _, healthy := s.SessionHealth(client); !healthy {
		t.Fatal("initial policy failed")
	}
	if len(s.db.policyPlans.items) == 0 {
		t.Fatal("real policy path did not prepare its query")
	}
	if err := changedOne(s.db.Exec(`UPDATE users SET role='admin' WHERE name=?`, client.Name)); err != nil {
		t.Fatal(err)
	}
	if u, healthy := s.SessionHealth(client); !healthy || u.Role != "admin" {
		t.Fatal("cached plan returned old actor role")
	}
	if err := setCredentialEpochQ(s.db, client.Name, 1); err != nil {
		t.Fatal(err)
	}
	if _, healthy := s.SessionHealth(client); healthy {
		t.Fatal("prepared authorization retained a revoked epoch")
	}
	if err := setCredentialEpochQ(s.db, client.Name, 0); err != nil {
		t.Fatal(err)
	}
	if _, healthy := s.SessionHealth(client); !healthy {
		t.Fatal("restored synthetic epoch did not read freshly")
	}
	if _, err := s.db.Exec(`DROP TABLE credential_versions`); err != nil {
		t.Fatal(err)
	}
	if _, healthy := s.SessionHealth(client); healthy {
		t.Fatal("prepared policy schema failure was not fail closed")
	}
}

func TestReadStatementsCapacityAndCancellationKeepOriginalDeadline(t *testing.T) {
	s := testStore(t)
	cache := newReadStatements(s.readDB(), 2)
	t.Cleanup(cache.close)
	for i := 0; i < 6; i++ {
		query := fmt.Sprintf("SELECT ? /* fixed synthetic shape %d */", i)
		var got int
		if err := cachedReadRow(t.Context(), s.readDB(), cache, query, i).Scan(&got); err != nil || got != i {
			t.Fatal(got, err)
		}
	}
	if len(cache.items) != 2 {
		t.Fatal("statement count exceeded its hard capacity", len(cache.items))
	}
	// A fresh miss cannot wait on another Prepare beyond the caller's budget.
	miss := newReadStatements(s.readDB(), 2)
	t.Cleanup(miss.close)
	miss.prepare <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := miss.statement(ctx, `SELECT ?`)
	<-miss.prepare
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second || len(miss.items) != 0 {
		t.Fatal("prepare gate ignored the original context", err)
	}
	if _, err := miss.statement(t.Context(), `SELECT missing_column FROM missing_read_plan_table`); err == nil || len(miss.items) != 0 {
		t.Fatal("failed Prepare was cached or retried as success", err)
	}
	ctx2, cancel2 := context.WithCancel(t.Context())
	cancel2()
	var value int
	if err := cachedReadRow(ctx2, s.readDB(), cache, `SELECT ? /* fixed synthetic shape 0 */`, 9).Scan(&value); !errors.Is(err, context.Canceled) {
		t.Fatal("cache hit bypassed cancellation", err)
	}
}

func TestReadStatementsCloseWhilePrepareWaitsDoesNotPublishOrLeak(t *testing.T) {
	s := testStore(t)
	cache := s.db.readPlans
	stmt, err := cache.statement(t.Context(), `SELECT ?`)
	if err != nil || stmt == nil {
		t.Fatal(err)
	}
	var held []*sql.Conn
	for range 2 {
		conn, err := s.readDB().Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
	}
	defer func() {
		for _, conn := range held {
			_ = conn.Close()
		}
	}()
	before := s.readDB().Stats().WaitCount
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := cache.statement(ctx, `SELECT ? + 1`); done <- err }()
	deadline := time.Now().Add(time.Second)
	for s.readDB().Stats().WaitCount == before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.readDB().Stats().WaitCount == before {
		t.Fatal("new Prepare did not reach the occupied reader pool")
	}
	cache.close()
	for _, conn := range held {
		_ = conn.Close()
	}
	select {
	case err := <-done:
		if !errors.Is(err, sql.ErrConnDone) {
			t.Fatal("closed cache published a prepared statement", err)
		}
	case <-ctx.Done():
		t.Fatal("Prepare did not release after cache close")
	}
	if len(cache.items) != 0 || !cache.closed || s.readDB().Stats().InUse != 0 {
		t.Fatal("closed cache leaked statements/connections")
	}
	if _, err := stmt.QueryContext(t.Context(), 1); err == nil {
		t.Fatal("retained statement remained usable after close")
	}
}
