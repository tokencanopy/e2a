package identity

// SetExpiredDeleteBatchForTest overrides the DeleteExpiredMessages batch size for
// the duration of a test and returns a restore func. Lets the external identity_test
// package exercise the multi-batch drain loop cheaply (a few rows, tiny batch) instead
// of seeding >5000 rows. Compiled only under test.
func SetExpiredDeleteBatchForTest(n int64) (restore func()) {
	prev := expiredDeleteBatch
	expiredDeleteBatch = n
	return func() { expiredDeleteBatch = prev }
}

// SetThreadChildDetachBatchForTest shrinks the per-statement child-pointer
// rewrite bound so integration tests can prove a high-fanout parent drains
// across several committed maintenance transactions.
func SetThreadChildDetachBatchForTest(n int64) (restore func()) {
	prev := threadChildDetachBatch
	threadChildDetachBatch = n
	return func() { threadChildDetachBatch = prev }
}

// ThreadAnchorBatchQueryForTest exposes the exact production lookup so generic
// planner tests cannot silently drift to a simplified, index-friendly shape.
func ThreadAnchorBatchQueryForTest() string {
	return threadAnchorBatchQuery
}
