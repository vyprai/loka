package dbproxy

import (
	"testing"
)

func TestMySQLClassify_Truncate(t *testing.T) {
	mp := newMySQLProxy(nil)
	if mp.classifySQL("TRUNCATE TABLE logs") {
		t.Error("TRUNCATE should be write")
	}
}

func TestMySQLClassify_AlterTable(t *testing.T) {
	mp := newMySQLProxy(nil)
	if mp.classifySQL("ALTER TABLE users ADD COLUMN email VARCHAR(255)") {
		t.Error("ALTER should be write")
	}
}

func TestMySQLClassify_DropTable(t *testing.T) {
	mp := newMySQLProxy(nil)
	if mp.classifySQL("DROP TABLE IF EXISTS temp") {
		t.Error("DROP should be write")
	}
}

func TestMySQLClassify_CreateIndex(t *testing.T) {
	mp := newMySQLProxy(nil)
	if mp.classifySQL("CREATE INDEX idx_name ON users(name)") {
		t.Error("CREATE should be write")
	}
}

func TestMySQLClassify_Replace(t *testing.T) {
	mp := newMySQLProxy(nil)
	if mp.classifySQL("REPLACE INTO t VALUES (1, 'a')") {
		t.Error("REPLACE should be write")
	}
}

func TestMySQLClassify_Call(t *testing.T) {
	mp := newMySQLProxy(nil)
	if mp.classifySQL("CALL my_proc()") {
		t.Error("CALL should be write")
	}
}

func TestMySQLClassify_ShowTables(t *testing.T) {
	mp := newMySQLProxy(nil)
	if !mp.classifySQL("SHOW TABLES") {
		t.Error("SHOW should be read")
	}
}

func TestMySQLClassify_ShowStatus(t *testing.T) {
	mp := newMySQLProxy(nil)
	if !mp.classifySQL("SHOW STATUS") {
		t.Error("SHOW STATUS should be read")
	}
}

func TestMySQLClassify_DescribeTable(t *testing.T) {
	mp := newMySQLProxy(nil)
	// DESCRIBE is not currently recognized as read — routed to primary (safe default).
	if mp.classifySQL("DESCRIBE users") {
		t.Error("DESCRIBE not recognized — should default to write (primary)")
	}
}

func TestMySQLClassify_Explain(t *testing.T) {
	mp := newMySQLProxy(nil)
	if !mp.classifySQL("EXPLAIN SELECT * FROM users") {
		t.Error("EXPLAIN should be read")
	}
}

func TestMySQLClassify_TransactionCycle(t *testing.T) {
	mp := newMySQLProxy(nil)

	mp.classifySQL("BEGIN")
	if !mp.txnPinned {
		t.Error("BEGIN should pin")
	}

	// SELECT inside txn → write (pinned).
	if mp.classifySQL("SELECT 1") {
		t.Error("SELECT inside txn should go to primary")
	}

	mp.classifySQL("COMMIT")
	if mp.txnPinned {
		t.Error("COMMIT should unpin")
	}

	// After commit, SELECT → read.
	if !mp.classifySQL("SELECT 1") {
		t.Error("SELECT after COMMIT should be read")
	}
}

func TestMySQLClassify_RollbackUnpins(t *testing.T) {
	mp := newMySQLProxy(nil)
	mp.classifySQL("START TRANSACTION")
	if !mp.txnPinned {
		t.Error("START TRANSACTION should pin")
	}
	mp.classifySQL("ROLLBACK")
	if mp.txnPinned {
		t.Error("ROLLBACK should unpin")
	}
}

func TestMySQLClassify_SetAutocommit(t *testing.T) {
	mp := newMySQLProxy(nil)
	if mp.classifySQL("SET autocommit=0") {
		t.Error("SET should be write")
	}
}

func TestMySQLClassify_UseDatabase(t *testing.T) {
	mp := newMySQLProxy(nil)
	if mp.classifySQL("USE mydb") {
		t.Error("USE should be write (changes state)")
	}
}

func TestMySQLClassify_EmptyQuery(t *testing.T) {
	mp := newMySQLProxy(nil)
	if mp.classifySQL("") {
		t.Error("empty query should be write (safe default)")
	}
}

func TestMySQLClassify_CaseInsensitive(t *testing.T) {
	mp := newMySQLProxy(nil)
	if !mp.classifySQL("select * from users") {
		t.Error("lowercase select should be read")
	}
	if !mp.classifySQL("SELECT * from users") {
		t.Error("mixed case should be read")
	}
}

func TestMySQLClassify_LeadingWhitespace(t *testing.T) {
	mp := newMySQLProxy(nil)
	if !mp.classifySQL("  \t\n SELECT 1") {
		t.Error("SELECT with leading whitespace should be read")
	}
}

func TestMySQLClassify_WithComment(t *testing.T) {
	mp := newMySQLProxy(nil)
	// Comment stripping not implemented — comment prefix is not SELECT, so default to write.
	if mp.classifySQL("/* comment */ SELECT 1") {
		t.Error("comment prefix not stripped — should default to write")
	}
}
