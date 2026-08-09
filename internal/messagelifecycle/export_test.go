package messagelifecycle

// SetAccountMetricsWorkMemForTest and AccountMetricsWorkMemForTest expose the
// unexported work_mem seam for the external test package (Go's sanctioned
// export_test.go hook).
var SetAccountMetricsWorkMemForTest = setAccountMetricsWorkMem

const AccountMetricsWorkMemForTest = accountMetricsWorkMem
