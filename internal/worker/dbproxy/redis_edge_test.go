package dbproxy

import (
	"testing"
)

func TestRedisClassify_ExecWithoutMulti(t *testing.T) {
	rp := newRedisProxy(nil)
	// EXEC without prior MULTI — not in transaction, treated as write.
	isRead := rp.classifyCommand("EXEC")
	if isRead {
		t.Error("EXEC should be write")
	}
	if rp.txnPinned {
		t.Error("txnPinned should remain false after EXEC without MULTI")
	}
}

func TestRedisClassify_NestedMulti(t *testing.T) {
	rp := newRedisProxy(nil)
	rp.classifyCommand("MULTI")
	if !rp.txnPinned {
		t.Fatal("should be pinned after MULTI")
	}

	// Second MULTI inside transaction — should stay pinned (no-op).
	rp.classifyCommand("MULTI")
	if !rp.txnPinned {
		t.Error("nested MULTI should keep txnPinned true")
	}

	// EXEC should unpin.
	rp.classifyCommand("EXEC")
	if rp.txnPinned {
		t.Error("EXEC should unpin")
	}
}

func TestRedisClassify_DiscardUnpins(t *testing.T) {
	rp := newRedisProxy(nil)
	rp.classifyCommand("MULTI")
	rp.classifyCommand("DISCARD")
	if rp.txnPinned {
		t.Error("DISCARD should unpin transaction")
	}
}

func TestRedisClassify_SubscribeIsPinned(t *testing.T) {
	rp := newRedisProxy(nil)
	if rp.classifyCommand("SUBSCRIBE") {
		t.Error("SUBSCRIBE should be write (pinned to primary)")
	}
	if rp.classifyCommand("PSUBSCRIBE") {
		t.Error("PSUBSCRIBE should be write")
	}
}

func TestRedisClassify_AllReadCommands(t *testing.T) {
	rp := newRedisProxy(nil)
	reads := []string{
		"GET", "MGET", "STRLEN", "GETRANGE",
		"HGET", "HMGET", "HGETALL", "HKEYS",
		"LRANGE", "LLEN", "LINDEX",
		"SCARD", "SISMEMBER", "SMEMBERS",
		"ZCARD", "ZCOUNT", "ZRANGE", "ZRANK",
		"EXISTS", "TYPE", "TTL", "PTTL",
		"KEYS", "SCAN", "DBSIZE",
		"XRANGE", "XREVRANGE", "XLEN",
		"PFCOUNT", "BITCOUNT", "BITPOS", "GETBIT",
		"PING", "ECHO", "TIME", "INFO",
	}
	for _, cmd := range reads {
		if !rp.classifyCommand(cmd) {
			t.Errorf("%s should be classified as read", cmd)
		}
	}
}

func TestRedisClassify_WriteCommands(t *testing.T) {
	rp := newRedisProxy(nil)
	writes := []string{
		"SET", "MSET", "DEL", "INCR", "DECR",
		"HSET", "HDEL", "LPUSH", "RPUSH", "SADD",
		"ZADD", "EXPIRE", "PERSIST", "RENAME",
		"FLUSHDB", "FLUSHALL",
	}
	for _, cmd := range writes {
		if rp.classifyCommand(cmd) {
			t.Errorf("%s should be classified as write", cmd)
		}
	}
}

func TestRedisClassify_InsideMulti_AllWrite(t *testing.T) {
	rp := newRedisProxy(nil)
	rp.classifyCommand("MULTI")

	// All commands inside transaction should be write (go to primary).
	if rp.classifyCommand("GET") {
		t.Error("GET inside MULTI should be routed to primary (write)")
	}
	if rp.classifyCommand("PING") {
		t.Error("PING inside MULTI should be routed to primary")
	}
}

func TestRedisClassify_CaseInsensitive(t *testing.T) {
	rp := newRedisProxy(nil)
	if !rp.classifyCommand("get") {
		t.Error("lowercase 'get' should be read")
	}
	if !rp.classifyCommand("Get") {
		t.Error("mixed case 'Get' should be read")
	}
}
