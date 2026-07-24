package dal

// Stats contains aggregated message statistics.
// Shared by CountEmailStats and CountSMSStats.
type Stats struct {
	Total       int64
	Sent        int64
	Failed      int64
	SuccessRate float64
}
