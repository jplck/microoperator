package state

// Limits apply to the whole activation, including threads and descendants.
// A nil limits field is only for the existing reviewed-code path, never a
// substitute when an untrusted launch requests these controls.
type ResourceLimits struct {
	MemoryBytes    int64 `json:"memory_bytes"`
	WorkspaceBytes int64 `json:"workspace_bytes"`
	Processes      int64 `json:"processes"`
	CPUPercent     int64 `json:"cpu_percent"`
}
