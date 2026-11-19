//go:build !windows

package procjob

// Manager is a no-op process lifecycle manager on non-Windows platforms.
type Manager struct{}

// New returns a no-op Manager on non-Windows platforms.
func New() (*Manager, error) {
	return &Manager{}, nil
}

// Assign is a no-op on non-Windows platforms.
func (m *Manager) Assign(pid int) error {
	return nil
}

// Close is a no-op on non-Windows platforms.
func (m *Manager) Close() error {
	return nil
}
