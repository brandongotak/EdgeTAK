package mission

import "slices"

// LatestCoT returns the most recent CoT XML for uid if c may see it: either
// c shares a group with the client that sent it, or c can read a mission
// that contains the item. It backs /Marti/api/cot/xml/{uid}, which clients
// call to fetch items announced in mission change notifications.
func (s *Service) LatestCoT(uid string, c Caller) (string, bool) {
	if ev, groups := s.latest.lookup(uid); ev != nil {
		if c.Admin || slices.ContainsFunc(groups, func(g string) bool { return slices.Contains(c.Groups, g) }) {
			return string(ev.XML()), true
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range s.missions {
		if x, ok := m.CoT[uid]; ok && Can(s.role(m, c), PermRead) {
			return x, true
		}
	}
	return "", false
}
