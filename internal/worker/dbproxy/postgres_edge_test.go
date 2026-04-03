package dbproxy

import (
	"testing"
)

func TestPostgresClassify_LockTable(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if pp.classifySQL("LOCK TABLE users IN ACCESS EXCLUSIVE MODE") {
		t.Error("LOCK TABLE should be classified as write")
	}
}

func TestPostgresClassify_CTEWithInsert(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	sql := "WITH inserted AS (INSERT INTO t VALUES (1) RETURNING *) SELECT * FROM inserted"
	// CTE with side effects — should be write.
	if pp.classifySQL(sql) {
		t.Error("CTE with INSERT should be classified as write")
	}
}

func TestPostgresClassify_VacuumAnalyze(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if pp.classifySQL("VACUUM ANALYZE users") {
		t.Error("VACUUM should be write")
	}
	if pp.classifySQL("ANALYZE users") {
		t.Error("ANALYZE should be write")
	}
}

func TestPostgresClassify_CreateIndex(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if pp.classifySQL("CREATE INDEX idx_name ON users(name)") {
		t.Error("CREATE should be write")
	}
}

func TestPostgresClassify_DropTable(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if pp.classifySQL("DROP TABLE IF EXISTS temp_data") {
		t.Error("DROP should be write")
	}
}

func TestPostgresClassify_Truncate(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if pp.classifySQL("TRUNCATE TABLE logs") {
		t.Error("TRUNCATE should be write")
	}
}

func TestPostgresClassify_AlterTable(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if pp.classifySQL("ALTER TABLE users ADD COLUMN email TEXT") {
		t.Error("ALTER should be write")
	}
}

func TestPostgresClassify_Savepoint(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if pp.classifySQL("SAVEPOINT sp1") {
		t.Error("SAVEPOINT should be write")
	}
}

func TestPostgresClassify_TransactionPinning_FullCycle(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}

	// Begin pins.
	if pp.classifySQL("BEGIN") {
		t.Error("BEGIN should be write")
	}
	if !pp.txnPinned {
		t.Error("should be pinned after BEGIN")
	}

	// Select inside txn goes to primary.
	if pp.classifySQL("SELECT 1") {
		t.Error("SELECT inside txn should go to primary")
	}

	// Commit unpins.
	pp.classifySQL("COMMIT")
	if pp.txnPinned {
		t.Error("COMMIT should unpin")
	}

	// Select after commit goes to replica.
	if !pp.classifySQL("SELECT 1") {
		t.Error("SELECT after COMMIT should be read")
	}
}

func TestPostgresClassify_RollbackUnpins(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	pp.classifySQL("BEGIN")
	pp.classifySQL("ROLLBACK")
	if pp.txnPinned {
		t.Error("ROLLBACK should unpin")
	}
}

func TestPostgres_PrepStmtEviction(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}

	// Fill to capacity.
	for i := 0; i < 1001; i++ {
		pp.prepStmts[string(rune(i))] = true
	}

	// Parse a new statement — should evict one to make room.
	payload := "new_stmt\x00SELECT 1\x00"
	pp.classifyMessage('P', []byte(payload))

	if len(pp.prepStmts) > 1001 {
		t.Errorf("expected eviction to keep map bounded, got %d entries", len(pp.prepStmts))
	}
	if _, ok := pp.prepStmts["new_stmt"]; !ok {
		t.Error("new statement should be in map after eviction")
	}
}

func TestPostgres_BindUsesStoredClassification(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}

	// Parse a SELECT statement.
	pp.classifyMessage('P', []byte("my_select\x00SELECT * FROM users\x00"))

	// Bind should use stored classification.
	isRead := pp.classifyMessage('B', []byte("portal\x00my_select\x00"))
	if !isRead {
		t.Error("Bind to SELECT prepared stmt should be read")
	}

	// Parse an INSERT.
	pp.classifyMessage('P', []byte("my_insert\x00INSERT INTO t VALUES(1)\x00"))

	isRead = pp.classifyMessage('B', []byte("portal\x00my_insert\x00"))
	if isRead {
		t.Error("Bind to INSERT prepared stmt should be write")
	}
}
