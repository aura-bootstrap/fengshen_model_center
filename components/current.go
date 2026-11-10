package components

// CachedCurrent checks only metadata for an already observed build. It never
// enumerates directories, reads file contents, hashes, or executes components.
// Large/unverified builds remain unknown until an explicit bounded observation.
// Metadata cannot detect same-size, same-mtime in-place content changes.
func (m *Manager) CachedCurrent(e Evidence) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := e.ComponentID + "/" + e.Version
	c, ok := m.verified[key]
	if !ok {
		c, ok = m.reports[key]
	}
	if !ok || c.manifest == "" || c.manifest != e.ManifestSHA256 || len(c.entries) == 0 || len(c.entries) > 256 {
		return false
	}
	s, err := m.scope()
	if err != nil {
		return false
	}
	defer s.Close()
	for _, before := range c.entries {
		f, after, err := open(s, before.Relative)
		if f != nil {
			f.Close()
		}
		if err != nil || before.Identity == "" || !same(before, after) {
			return false
		}
	}
	return true
}
