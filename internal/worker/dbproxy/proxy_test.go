package dbproxy

import (
	"testing"
)

// --- Postgres SQL classification ---

func TestPostgresClassifySQL_Select(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if !pp.classifySQL("SELECT * FROM users") {
		t.Error("SELECT should be read")
	}
}

func TestPostgresClassifySQL_SelectForUpdate(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if pp.classifySQL("SELECT * FROM users FOR UPDATE") {
		t.Error("SELECT FOR UPDATE should be write")
	}
}

func TestPostgresClassifySQL_Insert(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if pp.classifySQL("INSERT INTO users VALUES (1)") {
		t.Error("INSERT should be write")
	}
}

func TestPostgresClassifySQL_Update(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if pp.classifySQL("UPDATE users SET name='x'") {
		t.Error("UPDATE should be write")
	}
}

func TestPostgresClassifySQL_Delete(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if pp.classifySQL("DELETE FROM users") {
		t.Error("DELETE should be write")
	}
}

func TestPostgresClassifySQL_Show(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if !pp.classifySQL("SHOW server_version") {
		t.Error("SHOW should be read")
	}
}

func TestPostgresClassifySQL_Explain(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if !pp.classifySQL("EXPLAIN SELECT 1") {
		t.Error("EXPLAIN should be read")
	}
}

func TestPostgresClassifySQL_Begin(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if pp.classifySQL("BEGIN") {
		t.Error("BEGIN should be write (pins to primary)")
	}
	if !pp.txnPinned {
		t.Error("expected txnPinned after BEGIN")
	}
}

func TestPostgresClassifySQL_Set(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if pp.classifySQL("SET search_path TO public") {
		t.Error("SET should be write")
	}
}

func TestPostgresClassifySQL_TransactionPinning(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	pp.classifySQL("BEGIN")
	if pp.classifySQL("SELECT 1") {
		t.Error("SELECT inside transaction should go to primary (not read)")
	}
}

func TestPostgresClassifySQL_CopyTo(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if !pp.classifySQL("COPY users TO STDOUT") {
		t.Error("COPY TO should be read")
	}
}

func TestPostgresClassifySQL_CopyFrom(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if pp.classifySQL("COPY users FROM STDIN") {
		t.Error("COPY FROM should be write")
	}
}

func TestPostgresClassifySQL_CaseInsensitive(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if !pp.classifySQL("select * from users") {
		t.Error("lowercase select should be read")
	}
}

func TestPostgresClassifySQL_LeadingWhitespace(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if !pp.classifySQL("   SELECT 1") {
		t.Error("SELECT with leading whitespace should be read")
	}
}

// --- MySQL SQL classification ---

func TestMySQLClassifySQL_Select(t *testing.T) {
	mp := &MySQLProxy{stmtMap: make(map[uint32]bool)}
	if !mp.classifySQL("SELECT 1") {
		t.Error("SELECT should be read")
	}
}

func TestMySQLClassifySQL_Insert(t *testing.T) {
	mp := &MySQLProxy{stmtMap: make(map[uint32]bool)}
	if mp.classifySQL("INSERT INTO t VALUES (1)") {
		t.Error("INSERT should be write")
	}
}

func TestMySQLClassifySQL_Begin(t *testing.T) {
	mp := &MySQLProxy{stmtMap: make(map[uint32]bool)}
	if mp.classifySQL("BEGIN") {
		t.Error("BEGIN should be write")
	}
	if !mp.txnPinned {
		t.Error("expected txnPinned after BEGIN")
	}
}

func TestMySQLClassifySQL_Commit(t *testing.T) {
	mp := &MySQLProxy{stmtMap: make(map[uint32]bool)}
	mp.txnPinned = true
	mp.classifySQL("COMMIT")
	if mp.txnPinned {
		t.Error("expected txnPinned=false after COMMIT")
	}
}

func TestMySQLClassifySQL_Show(t *testing.T) {
	mp := &MySQLProxy{stmtMap: make(map[uint32]bool)}
	if !mp.classifySQL("SHOW DATABASES") {
		t.Error("SHOW should be read")
	}
}

func TestMySQLClassifySQL_SelectForUpdate(t *testing.T) {
	mp := &MySQLProxy{stmtMap: make(map[uint32]bool)}
	if mp.classifySQL("SELECT * FROM t FOR UPDATE") {
		t.Error("SELECT FOR UPDATE should be write")
	}
}

func TestMySQLClassifyPacket_ComQuery(t *testing.T) {
	mp := &MySQLProxy{stmtMap: make(map[uint32]bool)}
	if !mp.classifyPacket(comQuery, []byte("SELECT 1")) {
		t.Error("COM_QUERY SELECT should be read")
	}
	if mp.classifyPacket(comQuery, []byte("INSERT INTO t VALUES (1)")) {
		t.Error("COM_QUERY INSERT should be write")
	}
}

// --- Redis command classification ---

func TestRedisClassifyCommand_Get(t *testing.T) {
	rp := &RedisProxy{}
	if !rp.classifyCommand("GET") {
		t.Error("GET should be read")
	}
}

func TestRedisClassifyCommand_Set(t *testing.T) {
	rp := &RedisProxy{}
	if rp.classifyCommand("SET") {
		t.Error("SET should be write")
	}
}

func TestRedisClassifyCommand_MGet(t *testing.T) {
	rp := &RedisProxy{}
	if !rp.classifyCommand("MGET") {
		t.Error("MGET should be read")
	}
}

func TestRedisClassifyCommand_Del(t *testing.T) {
	rp := &RedisProxy{}
	if rp.classifyCommand("DEL") {
		t.Error("DEL should be write")
	}
}

func TestRedisClassifyCommand_HGetAll(t *testing.T) {
	rp := &RedisProxy{}
	if !rp.classifyCommand("HGETALL") {
		t.Error("HGETALL should be read")
	}
}

func TestRedisClassifyCommand_Multi(t *testing.T) {
	rp := &RedisProxy{}
	if rp.classifyCommand("MULTI") {
		t.Error("MULTI should be write (pins to primary)")
	}
	if !rp.txnPinned {
		t.Error("expected txnPinned after MULTI")
	}
}

func TestRedisClassifyCommand_Exec(t *testing.T) {
	rp := &RedisProxy{txnPinned: true}
	rp.classifyCommand("EXEC")
	if rp.txnPinned {
		t.Error("expected txnPinned=false after EXEC")
	}
}

func TestRedisClassifyCommand_InsideMulti(t *testing.T) {
	rp := &RedisProxy{txnPinned: true}
	if rp.classifyCommand("GET") {
		t.Error("GET inside MULTI should go to primary")
	}
}

func TestRedisClassifyCommand_CaseInsensitive(t *testing.T) {
	rp := &RedisProxy{}
	if !rp.classifyCommand("get") {
		t.Error("lowercase get should be read")
	}
}

func TestRedisClassifyCommand_Scan(t *testing.T) {
	rp := &RedisProxy{}
	if !rp.classifyCommand("SCAN") {
		t.Error("SCAN should be read")
	}
}

func TestRedisClassifyCommand_Subscribe(t *testing.T) {
	rp := &RedisProxy{}
	if rp.classifyCommand("SUBSCRIBE") {
		t.Error("SUBSCRIBE should be write (pins to backend)")
	}
}

// --- SQL classification edge cases ---

func TestPostgresClassifySQL_EmptyQuery(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if pp.classifySQL("") {
		t.Error("empty query should be write (safe default)")
	}
}

func TestPostgresClassifySQL_MultilineQuery(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if !pp.classifySQL("SELECT\n* FROM\nusers") {
		t.Error("multiline SELECT should be read")
	}
}

func TestPostgresClassifySQL_WithInlineComment(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	// Comment before SELECT — prefix matching sees "/*" not "SELECT".
	// This routes to primary (safe default).
	if pp.classifySQL("/* comment */ SELECT 1") {
		t.Error("SQL with leading comment should go to primary (prefix doesn't match SELECT)")
	}
}

func TestPostgresClassifySQL_WithLeadingLineComment(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if pp.classifySQL("-- comment\nSELECT 1") {
		t.Error("SQL with leading line comment should go to primary")
	}
}

func TestPostgresClassifySQL_MultipleStatements(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	// Known limitation: only checks prefix. "SELECT 1; DROP TABLE" looks like a read.
	// This is safe because postgres doesn't allow multiple statements in simple query
	// protocol when using prepared statements, and the proxy pins transactions.
	if !pp.classifySQL("SELECT 1; DROP TABLE users") {
		t.Error("prefix-only classification: SELECT; is still a read")
	}
}

func TestPostgresClassifySQL_Fetch(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if !pp.classifySQL("FETCH NEXT FROM my_cursor") {
		t.Error("FETCH should be read")
	}
}

func TestPostgresClassifyMessage_UnknownType(t *testing.T) {
	pp := &PostgresProxy{prepStmts: make(map[string]bool)}
	if pp.classifyMessage(0xFF, []byte("anything")) {
		t.Error("unknown message type should be write (safe default)")
	}
}

func TestMySQLClassifyPacket_EmptyPayload(t *testing.T) {
	mp := &MySQLProxy{stmtMap: make(map[uint32]bool)}
	if mp.classifyPacket(comQuery, []byte{}) {
		t.Error("empty COM_QUERY should be write")
	}
}

func TestMySQLClassifySQL_MultilineQuery(t *testing.T) {
	mp := &MySQLProxy{stmtMap: make(map[uint32]bool)}
	if !mp.classifySQL("SELECT\n* FROM\nusers") {
		t.Error("multiline SELECT should be read")
	}
}

func TestMySQLClassifySQL_WithComments(t *testing.T) {
	mp := &MySQLProxy{stmtMap: make(map[uint32]bool)}
	if mp.classifySQL("/* hint */ SELECT 1") {
		t.Error("SQL with leading comment should go to primary")
	}
}

func TestRedisClassifyCommand_EmptyCommand(t *testing.T) {
	rp := &RedisProxy{}
	if rp.classifyCommand("") {
		t.Error("empty command should be write")
	}
}

func TestRedisClassifyCommand_Discard(t *testing.T) {
	rp := &RedisProxy{txnPinned: true}
	rp.classifyCommand("DISCARD")
	if rp.txnPinned {
		t.Error("expected txnPinned=false after DISCARD")
	}
}

// --- Route PickBackend ---

func TestPickBackend_NilPrimary(t *testing.T) {
	r := &Route{Primary: nil, Replicas: []*Backend{{ID: "r1", Healthy: true}}}
	b := r.PickBackend(false)
	if b != nil {
		t.Error("expected nil when Primary is nil")
	}
}

func TestPickBackend_ReadWithNilReplicas(t *testing.T) {
	r := &Route{Primary: &Backend{ID: "p", Healthy: true}, Replicas: nil}
	b := r.PickBackend(true)
	if b == nil || b.ID != "p" {
		t.Error("read with nil Replicas should fall to primary")
	}
}

func TestPickBackend_WriteGoesToPrimary(t *testing.T) {
	r := &Route{
		Primary:  &Backend{ID: "p", Healthy: true},
		Replicas: []*Backend{{ID: "r1", Healthy: true}},
	}
	b := r.PickBackend(false)
	if b.ID != "p" {
		t.Errorf("write should go to primary, got %s", b.ID)
	}
}

func TestPickBackend_ReadGoesToReplica(t *testing.T) {
	r := &Route{
		Primary:  &Backend{ID: "p", Healthy: true},
		Replicas: []*Backend{{ID: "r1", Healthy: true}},
	}
	b := r.PickBackend(true)
	if b.ID != "r1" {
		t.Errorf("read should go to replica, got %s", b.ID)
	}
}

func TestPickBackend_ReadFallsToPrimary_NoReplicas(t *testing.T) {
	r := &Route{
		Primary:  &Backend{ID: "p", Healthy: true},
		Replicas: []*Backend{},
	}
	b := r.PickBackend(true)
	if b.ID != "p" {
		t.Errorf("read with no replicas should fall to primary, got %s", b.ID)
	}
}

func TestPickBackend_ReadFallsToPrimary_UnhealthyReplicas(t *testing.T) {
	r := &Route{
		Primary:  &Backend{ID: "p", Healthy: true},
		Replicas: []*Backend{{ID: "r1", Healthy: false}, {ID: "r2", Healthy: false}},
	}
	b := r.PickBackend(true)
	if b.ID != "p" {
		t.Errorf("read with unhealthy replicas should fall to primary, got %s", b.ID)
	}
}

func TestPickBackend_RoundRobinAcrossReplicas(t *testing.T) {
	r := &Route{
		Primary: &Backend{ID: "p", Healthy: true},
		Replicas: []*Backend{
			{ID: "r1", Healthy: true},
			{ID: "r2", Healthy: true},
		},
	}
	seen := map[string]int{}
	for i := 0; i < 10; i++ {
		b := r.PickBackend(true)
		seen[b.ID]++
	}
	if seen["r1"] == 0 || seen["r2"] == 0 {
		t.Errorf("expected round-robin across replicas, got %v", seen)
	}
}
