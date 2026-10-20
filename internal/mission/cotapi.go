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

// MissionInvitations lists a mission's invitations (official MissionInvitation JSON).
func (s *Service) MissionInvitations(name string, c Caller) ([]map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, _, err := s.require(name, c, PermRead)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, inv := range m.Invitations {
		out = append(out, map[string]any{
			"missionName": m.Name, "missionGuid": m.GUID, "invitee": inv.Invitee, "type": inv.Type,
			"creatorUid": inv.CreatorUID, "createTime": fmtTime(inv.Created), "role": roleJSON(inv.Role),
		})
	}
	return out, nil
}

// ReadToken issues a token carrying the caller's current role on the mission.
func (s *Service) ReadToken(name string, c Caller) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, role, err := s.require(name, c, PermRead)
	if err != nil {
		return "", err
	}
	return s.newToken(m, c.ClientUID, role), nil
}

// SubscriberUIDs lists a mission's subscribers for a caller with read access.
func (s *Service) SubscriberUIDs(name string, c Caller) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, _, err := s.require(name, c, PermRead)
	if err != nil {
		return nil, err
	}
	return subscriberUIDs(m), nil
}

// RenameGroup follows a group rename in every mission's group list.
func (s *Service) RenameGroup(old, new string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.missions {
		if i := slices.Index(m.Groups, old); i >= 0 {
			m.Groups[i] = new
			if err := s.save(m); err != nil {
				s.log.Error("save mission", "mission", m.Name, "err", err)
			}
		}
	}
}
